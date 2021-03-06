package accumulator

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// BatchProof :
type BatchProof struct {
	Targets []uint64
	Proof   []Hash
	// list of leaf locations to delete, along with a bunch of hashes that give the proof.
	// the position of the hashes is implied / computable from the leaf positions
}

// ToBytes give the bytes for a BatchProof.  It errors out silently because
// I don't think the binary.Write errors ever actually happen
func (bp *BatchProof) ToBytes() []byte {
	var buf bytes.Buffer

	// first write the number of targets (4 byte uint32)
	numTargets := uint32(len(bp.Targets))
	if numTargets == 0 {
		return nil
	}
	err := binary.Write(&buf, binary.BigEndian, numTargets)
	if err != nil {
		fmt.Printf("huh %s\n", err.Error())
		return nil
	}
	for _, t := range bp.Targets {
		// there's no need for these to be 64 bit for the next few decades...
		err := binary.Write(&buf, binary.BigEndian, t)
		if err != nil {
			fmt.Printf("huh %s\n", err.Error())
			return nil
		}
	}
	// then the rest is just hashes
	for _, h := range bp.Proof {
		_, err = buf.Write(h[:])
		if err != nil {
			fmt.Printf("huh %s\n", err.Error())
			return nil
		}
	}

	return buf.Bytes()
}

// ToString for debugging, shows the blockproof
func (bp *BatchProof) SortTargets() {
	sortUint64s(bp.Targets)
}

// ToString for debugging, shows the blockproof
func (bp *BatchProof) ToString() string {
	s := fmt.Sprintf("%d targets: ", len(bp.Targets))
	for _, t := range bp.Targets {
		s += fmt.Sprintf("%d\t", t)
	}
	s += fmt.Sprintf("\n%d proofs: ", len(bp.Proof))
	for _, p := range bp.Proof {
		s += fmt.Sprintf("%04x\t", p[:4])
	}
	s += "\n"
	return s
}

// FromBytesBatchProof gives a block proof back from the serialized bytes
func FromBytesBatchProof(b []byte) (BatchProof, error) {
	var bp BatchProof

	// if empty slice, return empty BatchProof with 0 targets
	if len(b) == 0 {
		return bp, nil
	}
	// otherwise, if there are less than 4 bytes we can't even see the number
	// of targets so something is wrong
	if len(b) < 4 {
		return bp, fmt.Errorf("batchproof only %d bytes", len(b))
	}

	buf := bytes.NewBuffer(b)
	// read 4 byte number of targets
	var numTargets uint32
	err := binary.Read(buf, binary.BigEndian, &numTargets)
	if err != nil {
		return bp, err
	}
	bp.Targets = make([]uint64, numTargets)
	for i, _ := range bp.Targets {
		err := binary.Read(buf, binary.BigEndian, &bp.Targets[i])
		if err != nil {
			return bp, err
		}
	}
	remaining := buf.Len()
	// the rest is hashes
	if remaining%32 != 0 {
		return bp, fmt.Errorf("%d bytes left, should be n*32", buf.Len())
	}
	bp.Proof = make([]Hash, remaining/32)

	for i, _ := range bp.Proof {
		copy(bp.Proof[i][:], buf.Next(32))
	}
	return bp, nil
}

// TODO OH WAIT -- this is not how to to it!  Don't hash all the way up to the
// roots to verify -- just hash up to any populated node!  Saves a ton of CPU!

// verifyBatchProof verifies a batchproof by checking against the set of known correct roots.
// Takes a BatchProof, the accumulator roots, and the number of leaves in the forest.
// Returns wether or not the proof verified correctly, the partial proof tree,
// and the subset of roots that was computed.
func verifyBatchProof(bp BatchProof, roots []Hash, numLeaves uint64,
	// cached should be a function that fetches nodes from the pollard and indicates whether they
	// exist or not, this is only useful for the pollard and nil should be passed for the forest.
	cached func(pos uint64) (bool, Hash)) (bool, [][3]node, []node) {
	if len(bp.Targets) == 0 {
		return true, nil, nil
	}

	if cached == nil {
		cached = func(_ uint64) (bool, Hash) { return false, empty }
	}

	rows := treeRows(numLeaves)
	proofPositions, computablePositions := ProofPositions(bp.Targets, numLeaves, rows)
	// targetNodes holds nodes that are known, on the bottom row those are the targets,
	// on the upper rows it holds computed nodes.
	// rootCandidates holds the roots that where computed, and have to be compared to the actual roots
	// at the end.
	targetNodes := make([]node, 0, len(bp.Targets)*int(rows))
	rootCandidates := make([]node, 0, len(roots))
	// trees is a slice of 3-Tuples, each tuple represents a parent and its children.
	// tuple[0] is the parent, tuple[1] is the left child and tuple[2] is the right child.
	// trees holds the entire proof tree of the batchproof in this way, sorted by the tuple[0].
	trees := make([][3]node, 0, len(computablePositions))
	// initialise the targetNodes for row 0.
	// TODO: this would be more straight forward if bp.Proofs wouldn't contain the targets
	proofHashes := make([]Hash, 0, len(proofPositions))
	targets := bp.Targets
	var targetsMatched uint64
	for len(targets) > 0 {
		// check if the target is the row 0 root.
		// this is the case if its the last leaf (pos==numLeaves-1)
		// AND the tree has a root at row 0 (numLeaves&1==1)
		if targets[0] == numLeaves-1 && numLeaves&1 == 1 {
			// target is the row 0 root, append it to the root candidates.
			rootCandidates = append(rootCandidates, node{Val: roots[0], Pos: targets[0]})
			bp.Proof = bp.Proof[1:]
			break
		}

		// `targets` might contain a target and its sibling or just the target, if
		// only the target is present the sibling will be in `proofPositions`.
		if uint64(len(proofPositions)) > targetsMatched &&
			targets[0]^1 == proofPositions[targetsMatched] {
			// the sibling of the target is included in the proof positions.
			lr := targets[0] & 1
			targetNodes = append(targetNodes, node{Pos: targets[0], Val: bp.Proof[lr]})
			proofHashes = append(proofHashes, bp.Proof[lr^1])
			targetsMatched++
			bp.Proof = bp.Proof[2:]
			targets = targets[1:]
			continue
		}

		// the sibling is not included in the proof positions, therefore it has to be included in `targets.
		// if there are less than 2 proof hashes or less than 2 targets left the proof is invalid
		// because there is a target without matching proof.
		if len(bp.Proof) < 2 || len(targets) < 2 {
			return false, nil, nil
		}

		targetNodes = append(targetNodes,
			node{Pos: targets[0], Val: bp.Proof[0]},
			node{Pos: targets[1], Val: bp.Proof[1]})
		bp.Proof = bp.Proof[2:]
		targets = targets[2:]
	}

	proofHashes = append(proofHashes, bp.Proof...)
	bp.Proof = proofHashes

	// hash every target node with its sibling (which either is contained in the proof or also a target)
	for len(targetNodes) > 0 {
		var target, proof node
		target = targetNodes[0]
		if len(proofPositions) > 0 && target.Pos^1 == proofPositions[0] {
			// target has a sibling in the proof positions, fetch proof
			proof = node{Pos: proofPositions[0], Val: bp.Proof[0]}
			proofPositions = proofPositions[1:]
			bp.Proof = bp.Proof[1:]
			targetNodes = targetNodes[1:]
		} else {
			// target should have its sibling in targetNodes
			if len(targetNodes) == 1 {
				// sibling not found
				return false, nil, nil
			}

			proof = targetNodes[1]
			targetNodes = targetNodes[2:]
		}

		// figure out which node is left and which is right
		left := target
		right := proof
		if target.Pos&1 == 1 {
			right, left = left, right
		}

		// get the hash of the parent from the cache or compute it
		parentPos := parent(target.Pos, rows)
		isParentCached, hash := cached(parentPos)
		if !isParentCached {
			hash = parentHash(left.Val, right.Val)
		}
		trees = append(trees, [3]node{{Val: hash, Pos: parentPos}, left, right})

		row := detectRow(parentPos, rows)
		if numLeaves&(1<<row) > 0 && parentPos == rootPosition(numLeaves, row, rows) {
			// the parent is a root -> store as candidate, to check against actual roots later.
			rootCandidates = append(rootCandidates, node{Val: hash, Pos: parentPos})
			continue
		}
		targetNodes = append(targetNodes, node{Val: hash, Pos: parentPos})
	}

	if len(rootCandidates) == 0 {
		// no roots to verify
		return false, nil, nil
	}

	// `roots` is ordered, therefore to verify that `rootCandidates` holds a subset of the roots
	// we count the roots that match in order.
	rootMatches := 0
	for _, root := range roots {
		if len(rootCandidates) > rootMatches && root == rootCandidates[rootMatches].Val {
			rootMatches++
		}
	}
	if len(rootCandidates) != rootMatches {
		// the proof is invalid because some root candidates were not included in `roots`.
		return false, nil, nil
	}

	return true, trees, rootCandidates
}

// Reconstruct takes a number of leaves and rows, and turns a block proof back
// into a partial proof tree. Should leave bp intact
func (bp *BatchProof) Reconstruct(
	numleaves uint64, forestRows uint8) (map[uint64]Hash, error) {

	if verbose {
		fmt.Printf("reconstruct blockproof %d tgts %d hashes nl %d fr %d\n",
			len(bp.Targets), len(bp.Proof), numleaves, forestRows)
	}
	proofTree := make(map[uint64]Hash)

	// If there is nothing to reconstruct, return empty map
	if len(bp.Targets) == 0 {
		return proofTree, nil
	}

	proofPositions, _ := ProofPositions(bp.Targets, numleaves, forestRows)
	proofPositions = mergeSortedSlices(bp.Targets, proofPositions)

	if len(proofPositions) != len(bp.Proof) {
		return nil, fmt.Errorf("invalid BatchProof, not enough proof hashes")
	}

	for i, pos := range proofPositions {
		proofTree[pos] = bp.Proof[i]
	}

	return proofTree, nil
}
