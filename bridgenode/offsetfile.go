package bridgenode

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/mit-dci/utreexo/util"
)

// buildOffsetFile builds an offsetFile which acts as an index
// for block locations since blk*.dat files generated by Bitcoin Core
// has blocks out of order.
//
// If you have more blk*.dat files to generate an index for, just
// delete the current offsetfile directory and run genproofs again.
// Fairly quick process with one blk*.dat file taking a few seconds.
//
// Returns the last block height that it processed.
func buildOffsetFile(dataDir string, tip util.Hash,
	cOffsetFile, cLastOffsetHeightFile string) (int32, error) {

	// Map to store Block Header Hashes for sorting purposes
	// blk*.dat files aren't in block order so this is needed
	nextMap := make(map[[32]byte]RawHeaderData)

	var offsetFile *os.File

	// If empty string is given, just use the default path
	// If not, then use the custom one given
	if cOffsetFile == "" {
		var err error
		offsetFile, err = os.OpenFile(util.OffsetFilePath,
			os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
	} else {
		var err error
		offsetFile, err = os.OpenFile(cOffsetFile,
			os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
	}

	lvdb := OpenIndexFile(dataDir)

	bufDB := BufferDB(lvdb)
	lvdb.Close()

	var lastOffsetHeight int32

	// Allocate buffered reader for readRawHeadersFromFile
	// Less overhead to pre allocate and reuse
	bufReader := bufio.NewReaderSize(nil, (1<<20)*128) // 128M
	wr := bufio.NewWriter(nil)

	defer offsetFile.Close()
	for fileNum := 0; ; fileNum++ {
		fileName := fmt.Sprintf("blk%05d.dat", fileNum)
		filePath := filepath.Join(dataDir, fileName)
		fmt.Printf("Building offsetfile... %s\n", fileName)

		_, err := os.Stat(filePath)
		if os.IsNotExist(err) {
			fmt.Printf("%s doesn't exist; done building\n", filePath)
			break
		}
		// grab headers from the .dat file as RawHeaderData type
		rawheaders, err := readRawHeadersFromFile(bufReader, filePath, uint32(fileNum), bufDB)
		if err != nil {
			panic(err)
		}
		tip, lastOffsetHeight, err = writeBlockOffset(
			rawheaders, nextMap, wr, offsetFile, lastOffsetHeight, tip)
		if err != nil {
			panic(err)
		}
	}

	// If empty string is given, just use the default path
	// If not, then use the custom one given
	if cLastOffsetHeightFile == "" {
		var err error
		// write the last height of the offsetfile
		// needed info for the main genproofs processes
		LastIndexOffsetHeightFile, err := os.OpenFile(
			util.LastIndexOffsetHeightFilePath, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
		defer LastIndexOffsetHeightFile.Close()
		// write to the file
		err = binary.Write(LastIndexOffsetHeightFile, binary.BigEndian, lastOffsetHeight)
		if err != nil {
			panic(err)
		}
	} else {
		var err error
		// write the last height of the offsetfile
		// needed info for the main genproofs processes
		LastIndexOffsetHeightFile, err := os.OpenFile(
			cLastOffsetHeightFile, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
		defer LastIndexOffsetHeightFile.Close()
		// write to the file
		err = binary.Write(LastIndexOffsetHeightFile, binary.BigEndian, lastOffsetHeight)
		if err != nil {
			panic(err)
		}
	}

	return lastOffsetHeight, nil
}

/*
Proof file format is somewhat like the blk.dat and rev.dat files.  But it's
always in order!  The offset file is in 8 byte chunks, so to find the proof
data for block 100 (really 101), seek to byte 800 and read 8 bytes.

The proof file is: 4 bytes empty (zeros for now, could do something else later)
4 bytes proof length, then the proof data.

Offset file is: 8 byte int64 offset.  Right now it's all 1 big file, can
change to 4 byte which file and 4 byte offset within file like the blk/rev but
we're not running on fat32 so works OK for now.
*/

// pFileWorker takes in blockproof and height information from the channel
// and writes to disk. MUST NOT have more than one worker as the proofs need to be
// in order
func proofWriterWorker(
	proofChan chan []byte, fileWait *sync.WaitGroup) {

	// for the pFile
	proofFile, err := os.OpenFile(
		util.PFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}

	offsetFile, err := os.OpenFile(
		util.POffsetFilePath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		panic(err)
	}

	_, err = offsetFile.Seek(0, 2)
	if err != nil {
		panic(err)
	}

	proofFileLocation, err := proofFile.Seek(0, 2)
	if err != nil {
		panic(err)

	}
	// TODO: optimization - don't write anything to proof file for blocks with
	// no deletions (inputs).  Lots of em in testnet.  Not so many on mainnet
	// I guess.  But in testnet would save millions *8 bytes.
	for {
		pbytes := <-proofChan
		// write to offset file first
		err = binary.Write(offsetFile, binary.BigEndian, proofFileLocation)
		if err != nil {
			fmt.Printf(err.Error())
			return
		}

		// write to proof file
		// first write big endian proof size int64
		err = binary.Write(proofFile, binary.BigEndian, int64(len(pbytes)))
		if err != nil {
			fmt.Printf(err.Error())
			return
		}
		proofFileLocation += 8

		// then write the proof
		written, err := proofFile.Write(pbytes)
		if err != nil {
			fmt.Printf(err.Error())
			return
		}
		proofFileLocation += int64(written)

		fileWait.Done()
	}
}

// readRawHeadersFromFile reads only the headers from the given .dat file
func readRawHeadersFromFile(bufReader *bufio.Reader, fileDir string, fileNum uint32, bufMap map[[32]byte]uint32) ([]RawHeaderData, error) {
	var blockHeaders []RawHeaderData

	f, err := os.Open(fileDir)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	fStat, err := f.Stat()
	if err != nil {
		panic(err)
	}
	fSize := fStat.Size()

	bufReader.Reset(f)

	var buf [88]byte    // buffer for magicbytes, size, and 80 byte header
	offset := uint32(0) // where the block is located from the beginning of the file

	// until offset is at the end of the file
	for int64(offset) != fSize {
		b := new(RawHeaderData)
		binary.BigEndian.PutUint32(b.FileNum[:], fileNum)
		binary.BigEndian.PutUint32(b.Offset[:], offset)

		_, err := bufReader.Read(buf[:])
		if err != nil {
			panic(err)
		}
		// check if Bitcoin magic bytes were read
		if util.CheckMagicByte(buf[:4]) == false {
			break
		}

		// read the 4 byte size of the load of the block
		size := binary.LittleEndian.Uint32(buf[4:8])

		// add 8bytes for the magic bytes (4bytes) and size (4bytes)
		offset = offset + size + uint32(8)

		copy(b.Prevhash[:], buf[12:12+32])

		// create block hash
		// double sha256 needed with Bitcoin
		first := sha256.Sum256(buf[8 : 8+80])
		b.CurrentHeaderHash = sha256.Sum256(first[:])

		// grab bitcoin core block index info
		b.UndoPos = bufMap[b.CurrentHeaderHash]
		if b.UndoPos == 0 {
			fmt.Printf("WARNING: block in blk file with header: %x\nexists without"+
				" a corresponding rev block. May be wasting disk space\n", b.CurrentHeaderHash)
		}

		// offset for the next block from the current position
		bufReader.Discard(int(size) - 80)

		blockHeaders = append(blockHeaders, *b)
	}

	return blockHeaders, nil
}

// Sorts and writes the block offset from the passed in blockHeaders.
func writeBlockOffset(
	blockHeaders []RawHeaderData, //        All headers from the select .dat file
	nextMap map[[32]byte]RawHeaderData, //  Map to save the current block hash
	wr *bufio.Writer, //buffered writer
	offsetFile *os.File, //                 File to save the sorted blocks and locations to
	tipnum int32, //                          Current block it's on
	tip util.Hash) ( //                Current hash of the block it's on
	util.Hash, int32, error) {

	wr.Reset(offsetFile)

	for _, b := range blockHeaders {
		if len(nextMap) > 10000 { //Just a random big number
			fmt.Println("Dead end tip. Exiting...")
			break
		}

		// The block's Prevhash doesn't match the
		// previous block header. Add to map.
		// Searches until it finds a hash that does.
		if b.Prevhash != tip {
			nextMap[b.Prevhash] = b
			continue
		}

		// Write the .dat file name and the
		// offset the block can be found at
		wr.Write(b.FileNum[:])
		wr.Write(b.Offset[:])

		undoOffset := make([]byte, 4)
		binary.BigEndian.PutUint32(undoOffset, b.UndoPos)

		// write undoblock offset
		wr.Write(undoOffset)

		// set the tip to current block's hash
		tip = b.CurrentHeaderHash
		tipnum++

		// check for next blocks in map
		// same thing but with the stored blocks
		// that we skipped over
		stashedBlock, ok := nextMap[tip]
		for ok {
			// Write the .dat file name and the
			// offset the block can be found at
			wr.Write(stashedBlock.FileNum[:])
			wr.Write(stashedBlock.Offset[:])

			// grab bitcoin core block index info
			//sCbIndex := GetBlockIndexInfo(stashedBlock.CurrentHeaderHash, lvdb)

			sUndoOffset := make([]byte, 4)
			binary.BigEndian.PutUint32(sUndoOffset, stashedBlock.UndoPos)

			// write undoblock offset
			wr.Write(sUndoOffset)

			// set the tip to current block's hash
			tip = stashedBlock.CurrentHeaderHash
			tipnum++

			// remove the written current block
			delete(nextMap, stashedBlock.Prevhash)

			// move to the next block
			stashedBlock, ok = nextMap[tip]
		}
	}
	wr.Flush()
	return tip, tipnum, nil
}
