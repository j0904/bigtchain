// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * @title CheckpointAnchor
 * @notice Anchors BIGT chain epoch checkpoints to an EVM L1 for weak subjectivity.
 *
 * Every epoch (~24h) a permissioned set of relayers submits a checkpoint:
 *   checkpoint = keccak256(epoch || blockHash || validatorSetHash)
 *
 * The contract verifies a 2/3+ weighted BLS multisig from the validator set
 * recorded in the *previous* checkpoint before accepting a new one.
 *
 * For this implementation the BLS verification is replaced by an m-of-n
 * ECDSA multisig over the checkpoint hash, using a permissioned relayer set.
 * A production deployment should replace _verifyRelayerSignatures with
 * on-chain BLS12-381 aggregate verification (EIP-2537).
 *
 * Security properties:
 *  - Checkpoints are append-only (cannot overwrite a finalised epoch).
 *  - Only addresses in the relayerSet may submit checkpoints.
 *  - Requires quorum (>= quorumRequired) relayer signatures.
 *  - Owner may add/remove relayers and change quorum with a timelock (not shown).
 */
contract CheckpointAnchor {
    // -------------------------------------------------------------------------
    // Types
    // -------------------------------------------------------------------------

    struct Checkpoint {
        uint64  epoch;
        bytes32 blockHash;
        bytes32 validatorSetHash;
        uint64  submittedAt;    // L1 block number
    }

    // -------------------------------------------------------------------------
    // State
    // -------------------------------------------------------------------------

    address public owner;
    uint256 public quorumRequired;

    mapping(address => bool) public isRelayer;
    address[] public relayers;

    /// @notice epoch => Checkpoint (immutable once set)
    mapping(uint64 => Checkpoint) public checkpoints;
    uint64 public latestEpoch;

    // -------------------------------------------------------------------------
    // Events
    // -------------------------------------------------------------------------

    event CheckpointAnchored(
        uint64  indexed epoch,
        bytes32         blockHash,
        bytes32         validatorSetHash,
        uint64          submittedAt
    );

    event RelayerAdded(address indexed relayer);
    event RelayerRemoved(address indexed relayer);
    event QuorumChanged(uint256 oldQuorum, uint256 newQuorum);

    // -------------------------------------------------------------------------
    // Errors
    // -------------------------------------------------------------------------

    error NotOwner();
    error NotRelayer(address sender);
    error EpochAlreadyAnchored(uint64 epoch);
    error EpochNotSequential(uint64 expected, uint64 got);
    error InsufficientSignatures(uint256 provided, uint256 required);
    error DuplicateSigner(address signer);
    error InvalidSignature();

    // -------------------------------------------------------------------------
    // Constructor
    // -------------------------------------------------------------------------

    constructor(address[] memory _relayers, uint256 _quorumRequired) {
        owner = msg.sender;
        quorumRequired = _quorumRequired;
        for (uint256 i = 0; i < _relayers.length; i++) {
            _addRelayer(_relayers[i]);
        }
    }

    // -------------------------------------------------------------------------
    // Core: submit checkpoint
    // -------------------------------------------------------------------------

    /**
     * @notice Submit an epoch checkpoint with relayer ECDSA multisig.
     * @param epoch            The BIGT epoch number (must be latestEpoch + 1).
     * @param blockHash        keccak256 of the epoch boundary block header.
     * @param validatorSetHash keccak256 of the current validator set.
     * @param signers          Ordered list of relayer addresses that signed.
     * @param signatures       Corresponding ECDSA signatures over the checkpoint hash.
     */
    function submitCheckpoint(
        uint64  epoch,
        bytes32 blockHash,
        bytes32 validatorSetHash,
        address[] calldata signers,
        bytes[] calldata signatures
    ) external {
        if (!isRelayer[msg.sender]) revert NotRelayer(msg.sender);
        if (checkpoints[epoch].submittedAt != 0) revert EpochAlreadyAnchored(epoch);
        if (epoch != latestEpoch + 1 && latestEpoch != 0) revert EpochNotSequential(latestEpoch + 1, epoch);
        if (signers.length != signatures.length) revert InsufficientSignatures(signers.length, quorumRequired);

        // Compute the message that signers committed to.
        bytes32 cpHash = _checkpointHash(epoch, blockHash, validatorSetHash);

        _verifyRelayerSignatures(cpHash, signers, signatures);

        Checkpoint storage cp = checkpoints[epoch];
        cp.epoch            = epoch;
        cp.blockHash        = blockHash;
        cp.validatorSetHash = validatorSetHash;
        cp.submittedAt      = uint64(block.number);

        latestEpoch = epoch;

        emit CheckpointAnchored(epoch, blockHash, validatorSetHash, cp.submittedAt);
    }

    // -------------------------------------------------------------------------
    // Queries
    // -------------------------------------------------------------------------

    /**
     * @notice Returns the latest anchored checkpoint. Reverts if none exists yet.
     */
    function getLatestCheckpoint() external view returns (Checkpoint memory) {
        require(latestEpoch > 0, "no checkpoint yet");
        return checkpoints[latestEpoch];
    }

    /**
     * @notice Returns the checkpoint for a specific epoch.
     */
    function getCheckpoint(uint64 epoch) external view returns (Checkpoint memory) {
        require(checkpoints[epoch].submittedAt != 0, "epoch not anchored");
        return checkpoints[epoch];
    }

    /**
     * @notice Computes the expected signing hash for a checkpoint.
     */
    function checkpointHash(
        uint64 epoch,
        bytes32 blockHash,
        bytes32 validatorSetHash
    ) external pure returns (bytes32) {
        return _checkpointHash(epoch, blockHash, validatorSetHash);
    }

    // -------------------------------------------------------------------------
    // Admin
    // -------------------------------------------------------------------------

    function addRelayer(address relayer) external {
        if (msg.sender != owner) revert NotOwner();
        _addRelayer(relayer);
    }

    function removeRelayer(address relayer) external {
        if (msg.sender != owner) revert NotOwner();
        require(isRelayer[relayer], "not a relayer");
        isRelayer[relayer] = false;
        emit RelayerRemoved(relayer);
    }

    function setQuorum(uint256 newQuorum) external {
        if (msg.sender != owner) revert NotOwner();
        emit QuorumChanged(quorumRequired, newQuorum);
        quorumRequired = newQuorum;
    }

    // -------------------------------------------------------------------------
    // Internal
    // -------------------------------------------------------------------------

    function _checkpointHash(
        uint64 epoch,
        bytes32 blockHash,
        bytes32 validatorSetHash
    ) internal pure returns (bytes32) {
        return keccak256(abi.encodePacked(
            "\x19BIGT Checkpoint\x00",
            epoch,
            blockHash,
            validatorSetHash
        ));
    }

    /**
     * @dev Verifies that `quorumRequired` distinct relayers signed `hash`.
     *      Uses eth_sign personal message format (EIP-191 prefix applied by signers).
     */
    function _verifyRelayerSignatures(
        bytes32 hash,
        address[] calldata signers,
        bytes[] calldata signatures
    ) internal view {
        if (signers.length < quorumRequired) {
            revert InsufficientSignatures(signers.length, quorumRequired);
        }
        // Eth signed message hash (EIP-191).
        bytes32 ethHash = keccak256(abi.encodePacked(
            "\x19Ethereum Signed Message:\n32",
            hash
        ));

        // Track seen signers to prevent duplicates.
        address lastSigner;
        for (uint256 i = 0; i < signers.length; i++) {
            address signer = signers[i];
            if (signer <= lastSigner) revert DuplicateSigner(signer);  // requires sorted input
            if (!isRelayer[signer]) revert NotRelayer(signer);

            (uint8 v, bytes32 r, bytes32 s) = _splitSignature(signatures[i]);
            address recovered = ecrecover(ethHash, v, r, s);
            if (recovered == address(0) || recovered != signer) revert InvalidSignature();

            lastSigner = signer;
        }
    }

    function _splitSignature(bytes memory sig) internal pure returns (uint8 v, bytes32 r, bytes32 s) {
        require(sig.length == 65, "bad sig length");
        assembly {
            r := mload(add(sig, 32))
            s := mload(add(sig, 64))
            v := byte(0, mload(add(sig, 96)))
        }
    }

    function _addRelayer(address relayer) internal {
        require(!isRelayer[relayer], "already a relayer");
        isRelayer[relayer] = true;
        relayers.push(relayer);
        emit RelayerAdded(relayer);
    }
}
