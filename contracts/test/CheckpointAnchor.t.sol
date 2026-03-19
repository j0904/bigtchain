// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test, console} from "forge-std/Test.sol";
import {CheckpointAnchor} from "../src/CheckpointAnchor.sol";

contract CheckpointAnchorTest is Test {
    CheckpointAnchor anchor;

    // Three relayer accounts derived deterministically from known private keys.
    uint256 constant PK1 = 0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80;
    uint256 constant PK2 = 0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d;
    uint256 constant PK3 = 0x5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a;

    address relayer1;
    address relayer2;
    address relayer3;

    function setUp() public {
        relayer1 = vm.addr(PK1);
        relayer2 = vm.addr(PK2);
        relayer3 = vm.addr(PK3);

        address[] memory relayerAddrs = new address[](3);
        relayerAddrs[0] = relayer1;
        relayerAddrs[1] = relayer2;
        relayerAddrs[2] = relayer3;

        anchor = new CheckpointAnchor(relayerAddrs, 2); // quorum = 2
    }

    // -------------------------------------------------------------------------
    // Helpers
    // -------------------------------------------------------------------------

    function _signCheckpoint(
        uint256 pk,
        uint64 epoch,
        bytes32 blockHash,
        bytes32 valSetHash
    ) internal view returns (bytes memory) {
        bytes32 cpHash = anchor.checkpointHash(epoch, blockHash, valSetHash);
        bytes32 ethHash = keccak256(abi.encodePacked(
            "\x19Ethereum Signed Message:\n32",
            cpHash
        ));
        (uint8 v, bytes32 r, bytes32 s) = vm.sign(pk, ethHash);
        return abi.encodePacked(r, s, v);
    }

    function _buildSortedSigners2(uint256 pkA, uint256 pkB)
        internal view
        returns (address[] memory signers, bytes[] memory sigs, uint64 epoch, bytes32 bh, bytes32 vsh)
    {
        epoch = 1;
        bh    = bytes32(uint256(0xdeadbeef));
        vsh   = bytes32(uint256(0xcafebabe));

        address addrA = vm.addr(pkA);
        address addrB = vm.addr(pkB);

        // Sort ascending (contract requires sorted signers).
        if (addrA > addrB) {
            (pkA, pkB) = (pkB, pkA);
            (addrA, addrB) = (addrB, addrA);
        }

        signers = new address[](2);
        sigs    = new bytes[](2);
        signers[0] = addrA;
        signers[1] = addrB;
        sigs[0] = _signCheckpoint(pkA, epoch, bh, vsh);
        sigs[1] = _signCheckpoint(pkB, epoch, bh, vsh);
    }

    // -------------------------------------------------------------------------
    // Tests
    // -------------------------------------------------------------------------

    function test_SubmitFirstCheckpoint_Success() public {
        (address[] memory signers, bytes[] memory sigs, uint64 epoch, bytes32 bh, bytes32 vsh)
            = _buildSortedSigners2(PK1, PK2);

        vm.prank(relayer1);
        anchor.submitCheckpoint(epoch, bh, vsh, signers, sigs);

        assertEq(anchor.latestEpoch(), 1);

        CheckpointAnchor.Checkpoint memory cp = anchor.getCheckpoint(1);
        assertEq(cp.epoch, 1);
        assertEq(cp.blockHash, bh);
        assertEq(cp.validatorSetHash, vsh);
        assertGt(cp.submittedAt, 0);
    }

    function test_SubmitSecondCheckpoint_Sequential() public {
        // Epoch 1
        (address[] memory s1, bytes[] memory sig1, uint64 e1, bytes32 bh1, bytes32 vsh1)
            = _buildSortedSigners2(PK1, PK2);
        vm.prank(relayer1);
        anchor.submitCheckpoint(e1, bh1, vsh1, s1, sig1);

        // Epoch 2
        uint64 epoch2 = 2;
        bytes32 bh2  = bytes32(uint256(0xbeefdead));
        bytes32 vsh2 = bytes32(uint256(0xfeedface));

        address addrA = relayer1 < relayer2 ? relayer1 : relayer2;
        address addrB = relayer1 < relayer2 ? relayer2 : relayer1;
        uint256 pkA   = relayer1 < relayer2 ? PK1 : PK2;
        uint256 pkB   = relayer1 < relayer2 ? PK2 : PK1;

        address[] memory signers = new address[](2);
        bytes[]   memory sigs    = new bytes[](2);
        signers[0] = addrA; signers[1] = addrB;
        sigs[0] = _signCheckpoint(pkA, epoch2, bh2, vsh2);
        sigs[1] = _signCheckpoint(pkB, epoch2, bh2, vsh2);

        vm.prank(relayer2);
        anchor.submitCheckpoint(epoch2, bh2, vsh2, signers, sigs);

        assertEq(anchor.latestEpoch(), 2);
    }

    function test_SubmitCheckpoint_NonSequential_Reverts() public {
        // Submit epoch 1 first.
        (address[] memory s1, bytes[] memory sig1, uint64 e1, bytes32 bh1, bytes32 vsh1)
            = _buildSortedSigners2(PK1, PK2);
        vm.prank(relayer1);
        anchor.submitCheckpoint(e1, bh1, vsh1, s1, sig1);

        // Try epoch 3 (skipping 2).
        (address[] memory s3, bytes[] memory sig3,,, ) = _buildSortedSigners2(PK2, PK3);
        // Build epoch3 sigs manually.
        uint64 epoch3 = 3;
        bytes32 bh3  = bytes32(uint256(0x1234));
        bytes32 vsh3 = bytes32(uint256(0x5678));

        address addrA = relayer2 < relayer3 ? relayer2 : relayer3;
        address addrB = relayer2 < relayer3 ? relayer3 : relayer2;
        uint256 pkA   = relayer2 < relayer3 ? PK2 : PK3;
        uint256 pkB   = relayer2 < relayer3 ? PK3 : PK2;

        s3 = new address[](2);
        s3[0] = addrA; s3[1] = addrB;
        sig3 = new bytes[](2);
        sig3[0] = _signCheckpoint(pkA, epoch3, bh3, vsh3);
        sig3[1] = _signCheckpoint(pkB, epoch3, bh3, vsh3);

        vm.prank(relayer1);
        vm.expectRevert(abi.encodeWithSelector(
            CheckpointAnchor.EpochNotSequential.selector, uint64(2), uint64(3)
        ));
        anchor.submitCheckpoint(epoch3, bh3, vsh3, s3, sig3);
    }

    function test_NonRelayer_Reverts() public {
        address attacker = makeAddr("attacker");
        vm.prank(attacker);
        vm.expectRevert(abi.encodeWithSelector(CheckpointAnchor.NotRelayer.selector, attacker));
        anchor.submitCheckpoint(1, bytes32(0), bytes32(0), new address[](0), new bytes[](0));
    }

    function test_DuplicateEpoch_Reverts() public {
        (address[] memory s, bytes[] memory sigs, uint64 epoch, bytes32 bh, bytes32 vsh)
            = _buildSortedSigners2(PK1, PK2);
        vm.prank(relayer1);
        anchor.submitCheckpoint(epoch, bh, vsh, s, sigs);

        vm.prank(relayer1);
        vm.expectRevert(abi.encodeWithSelector(CheckpointAnchor.EpochAlreadyAnchored.selector, epoch));
        anchor.submitCheckpoint(epoch, bh, vsh, s, sigs);
    }

    function test_InsufficientSignatures_Reverts() public {
        // Only 1 signature but quorum = 2.
        uint64 epoch = 1;
        bytes32 bh  = bytes32(uint256(0x1));
        bytes32 vsh = bytes32(uint256(0x2));

        address[] memory signers = new address[](1);
        bytes[]   memory sigs    = new bytes[](1);
        signers[0] = relayer1;
        sigs[0] = _signCheckpoint(PK1, epoch, bh, vsh);

        vm.prank(relayer1);
        vm.expectRevert(abi.encodeWithSelector(
            CheckpointAnchor.InsufficientSignatures.selector, uint256(1), uint256(2)
        ));
        anchor.submitCheckpoint(epoch, bh, vsh, signers, sigs);
    }

    function test_WrongSignature_Reverts() public {
        uint64 epoch = 1;
        bytes32 bh  = bytes32(uint256(0xabc));
        bytes32 vsh = bytes32(uint256(0xdef));

        // relayer1 signs for wrong epoch.
        address addrA = relayer1 < relayer2 ? relayer1 : relayer2;
        address addrB = relayer1 < relayer2 ? relayer2 : relayer1;
        uint256 pkA   = relayer1 < relayer2 ? PK1 : PK2;
        uint256 pkB   = relayer1 < relayer2 ? PK2 : PK1;

        address[] memory signers = new address[](2);
        bytes[]   memory sigs    = new bytes[](2);
        signers[0] = addrA; signers[1] = addrB;
        sigs[0] = _signCheckpoint(pkA, epoch, bh, vsh);
        sigs[1] = _signCheckpoint(pkB, 99 /* wrong epoch */, bh, vsh); // bad sig

        vm.prank(relayer1);
        vm.expectRevert(CheckpointAnchor.InvalidSignature.selector);
        anchor.submitCheckpoint(epoch, bh, vsh, signers, sigs);
    }

    function test_EmitsEvent_OnSuccess() public {
        (address[] memory signers, bytes[] memory sigs, uint64 epoch, bytes32 bh, bytes32 vsh)
            = _buildSortedSigners2(PK1, PK3);

        vm.prank(relayer1);
        vm.expectEmit(true, false, false, false);
        emit CheckpointAnchor.CheckpointAnchored(epoch, bh, vsh, 0);
        anchor.submitCheckpoint(epoch, bh, vsh, signers, sigs);
    }

    function test_AddRelayer_OnlyOwner() public {
        address newRelayer = makeAddr("new");
        anchor.addRelayer(newRelayer);
        assertTrue(anchor.isRelayer(newRelayer));

        address nonOwner = makeAddr("nonowner");
        vm.prank(nonOwner);
        vm.expectRevert(CheckpointAnchor.NotOwner.selector);
        anchor.addRelayer(makeAddr("x"));
    }

    function test_RemoveRelayer_OnlyOwner() public {
        anchor.removeRelayer(relayer3);
        assertFalse(anchor.isRelayer(relayer3));
    }

    function test_CheckpointHash_IsDeterministic() public view {
        bytes32 h1 = anchor.checkpointHash(5, bytes32(uint256(0xaaa)), bytes32(uint256(0xbbb)));
        bytes32 h2 = anchor.checkpointHash(5, bytes32(uint256(0xaaa)), bytes32(uint256(0xbbb)));
        assertEq(h1, h2);
    }

    function test_CheckpointHash_DifferentEpochs_DifferentHash() public view {
        bytes32 bh  = bytes32(uint256(0xfeed));
        bytes32 vsh = bytes32(uint256(0xface));
        bytes32 h1  = anchor.checkpointHash(1, bh, vsh);
        bytes32 h2  = anchor.checkpointHash(2, bh, vsh);
        assertTrue(h1 != h2);
    }

    function test_GetLatestCheckpoint_ReturnsCorrect() public {
        (address[] memory s, bytes[] memory sigs, uint64 epoch, bytes32 bh, bytes32 vsh)
            = _buildSortedSigners2(PK1, PK2);
        vm.prank(relayer1);
        anchor.submitCheckpoint(epoch, bh, vsh, s, sigs);

        CheckpointAnchor.Checkpoint memory latest = anchor.getLatestCheckpoint();
        assertEq(latest.epoch, epoch);
        assertEq(latest.blockHash, bh);
    }

    function test_GetLatestCheckpoint_NoCheckpoints_Reverts() public {
        vm.expectRevert("no checkpoint yet");
        anchor.getLatestCheckpoint();
    }
}
