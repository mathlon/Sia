package renter

// TODO: Make sure the `newMemory` channel for the renter is buffered out to one
// element.

// TODO: When building the chunk, need to also include a list of pieces that
// aren't properly replicated yet.

// TODO / NOTE: Once the filesystem is tree-based, instead of continually
// looping through the whole filesystem we can add values to the file metadata
// indicating the health of each folder + file, and the time of the last scan
// for each folder + file, where the folder scan time is the least recent time
// of any file in the folder, and the folder health is the lowest health of any
// file in the folder. This will allow us to go one folder at a time and focus
// on problem areas instead of doing everything all at once every iteration.
// This should boost scalability.

// TODO: If the original file would need to be downloaded to be repaired, don't
// repair data that has less than 25% of its redundant pieces. (so in a
// 10-of-110 situation, you repair when the piece count drops to 85 or lower).

import (
	"container/heap"
	"sync"
	"time"

	"github.com/NebulousLabs/Sia/types"
)

// ChunkHeap is a bunch of chunks sorted by percentage-completion for uploading.
// This is a temporary situation, once we have a filesystem we can do
// tree-diving instead to build out our chunk profile. This just simulates that.
type chunkHeap []*unfinishedChunk

// unfinishedChunk contains a chunk from the filesystem that has not finished
// uploading, including knowledge of the progress.
type unfinishedChunk struct {
	// Information about the file. localPath may be the empty string if the file
	// is known not to exist locally.
	renterFile *file
	localPath string

	// Information about the chunk, namely where it exists within the file.
	//
	// TODO / NOTE: As we change the file mapper, we're probably going to have
	// to update these fields. Compatibility shouldn't be an issue because this
	// struct is not persisted anywhere, it's always built from other
	// structures.
	index  uint64
	length uint64
	offset int64

	// The logical data is the data that is presented to the user when the user
	// requests the chunk. The physical data is all of the pieces that get
	// stored across the network.
	logicalChunkData  []byte
	physicalChunkData [][]byte

	// Fields for tracking the current progress of the chunk and all the pieces.
	memoryNeeded     uint64 // memory needed in bytes
	piecesNeeded     int // number of pieces to achieve a 100% complete upload
	piecesCompleted  int // number of pieces that have been fully uploaded.
	piecesRegistered int // number of pieces that are being uploaded, but aren't finished yet.
	pieceUsage       []bool // one per piece. 'false' = piece not uploaded. 'true' = piece uploaded.
	progress         float64 // percent complete at the moment uploading started
	unusedHosts      map[string]struct{} // hosts that aren't yet storing any pieces

	// Utilities.
	mu sync.Mutex
}

func (ch chunkHeap) Len() int            { return len(ch) }
func (ch chunkHeap) Less(i, j int) bool  { return ch[i].progress < ch[j].progress }
func (ch chunkHeap) Swap(i, j int)       { ch[i], ch[j] = ch[j], ch[i] }
func (ch *chunkHeap) Push(x interface{}) { *ch = append(*ch, x.(*unfinishedChunk)) }
func (ch *chunkHeap) Pop() interface{} {
	old := *ch
	n := len(old)
	x := old[n-1]
	*ch = old[0 : n-1]
	return x
}

// buildUnfinishedChunks will pull all of the unfinished chunks out of a file.
//
// TODO / NOTE: This code can be substantially simplified once the files store
// the HostPubKey instead of the FileContractID, and can be simplified even
// further once the layout is per-chunk instead of per-filecontract.
func (r *Renter) buildUnfinishedChunks(f *file, hosts map[string]struct{}, fcidToHPK map[types.FileContractID]types.SiaPublicKey) []*unfinishedChunk {
	// Files are not threadsafe.
	f.mu.Lock()
	defer f.mu.Unlock()

	// If the file is not being tracked, don't repair it.
	trackedFile, exists := r.tracking[f.name]
	if !exists {
		return nil
	}

	// Assemble the set of chunks.
	//
	// TODO / NOTE: Future files may have a different method for determining the
	// number of chunks. Changes will be made due to things like sparse files,
	// and the fact that chunks are going to be different sizes.
	chunkCount := f.numChunks()
	newUnfinishedChunks := make([]*unfinishedChunk, chunkCount)
	// Add a separate unusedHosts map for each chunk, as every chunk will have a
	// different set of unused hosts.
	for i := uint64(0); i < chunkCount; i++ {
		newUnfinishedChunks[i] = new(unfinishedChunk)
		newUnfinishedChunks[i].index = i
		newUnfinishedChunks[i].localPath = trackedFile.RepairPath

		// Mark the number of pieces needed for this chunk.
		newUnfinishedChunks[i].piecesNeeded = f.erasureCode.NumPieces()
		newUnfinishedChunks[i].memoryNeeded = f.pieceSize * uint64(f.erasureCode.NumPieces())

		// TODO / NOTE: Offset and length are going to have to be derived using
		// alternate means once chunks are no longer constant size within a
		// file. Likely the chunk metadata will contain this information, but we
		// also want to make sure that files are random-access, and don't
		// require seeking through a ton of chunk headers to get to an arbitrary
		// position. It's currently an open problem.
		newUnfinishedChunks[i].offset = int64(i * f.chunkSize())
		newUnfinishedChunks[i].length = f.chunkSize()

		// Fill out the set of unused hosts.
		newUnfinishedChunks[i].pieceUsage = make([]bool, f.erasureCode.NumPieces())
		newUnfinishedChunks[i].unusedHosts = make(map[string]struct{})
		for host := range hosts {
			newUnfinishedChunks[i].unusedHosts[host] = struct{}{}
		}
	}

	// Iterate through the contracts of the file and mark which hosts are
	// already in use for the chunk. As you delete hosts from the 'unusedHosts'
	// map, also increment the 'piecesCompleted' value.
	saveFile := false
	for fcid, fileContract := range f.contracts {
		// TODO: Need to figure out whether this contract line is still being
		// used. And even worse, need to figure out whether this particular
		// piece is still available in the contract, as hosts may drop
		// particular pieces or lose particular drives, resulting in the
		// contract line continuing to be valid but the data being gone.

		// Convert the FileContractID into a host pubkey using the host pubkey
		// lookup.
		hpk, exists := fcidToHPK[fileContract.ID]
		if !exists {
			// File contract does not seem to be part of the host anymore.
			// Delete this contract and mark the file to be saved.
			delete(f.contracts, fcid)
			saveFile = true
			continue
		}

		// Mark the chunk set based on the pieces in this contract.
		for _, piece := range fileContract.Pieces {
			_, exists := newUnfinishedChunks[piece.Chunk].unusedHosts[hpk.String()]
			nonRedundantPiece := newUnfinishedChunks[piece.Chunk].pieceUsage[piece.Piece]
			if exists && nonRedundantPiece {
				newUnfinishedChunks[piece.Chunk].pieceUsage[piece.Piece] = true
				newUnfinishedChunks[piece.Chunk].piecesCompleted++
				delete(newUnfinishedChunks[piece.Chunk].unusedHosts, hpk.String())
			} else if exists {
				// TODO / NOTE: This host has a piece, but it's the same piece
				// that another host has. We may want to take action (such as
				// deleting this piece from this host) because of this
				// inefficiency.
			}
		}
	}
	// If 'saveFile' is marked, it means we deleted some dead contracts and
	// cleaned up the file a bit. Save the file to clean up some space on disk
	// and prevent the same work from being repeated after the next restart.
	//
	// TODO / NOTE: This process isn't going to make sense anymore once we
	// switch to chunk-based saving.
	if saveFile {
		err := r.saveFile(f)
		if err != nil {
			r.log.Println("error while saving a file after pruning some contracts from it:", err)
		}
	}

	// Iterate through the set of newUnfinishedChunks and remove any that are
	// completed.
	totalIncomplete := 0
	for i := 0; i < len(newUnfinishedChunks); i++ {
		if newUnfinishedChunks[i].piecesCompleted < newUnfinishedChunks[i].piecesNeeded {
			newUnfinishedChunks[totalIncomplete] = newUnfinishedChunks[i]
			newUnfinishedChunks[i].progress = float64(newUnfinishedChunks[i].piecesCompleted) / float64(newUnfinishedChunks[i].piecesNeeded)
			totalIncomplete++
		}
	}
	newUnfinishedChunks = newUnfinishedChunks[:totalIncomplete]
	return newUnfinishedChunks
}

// managedBuildChunkHeap will iterate through all of the files in the renter and
// construct a chunk heap.
func (r *Renter) managedBuildChunkHeap(hosts map[string]struct{}, fcidToHPK map[types.FileContractID]types.SiaPublicKey) *chunkHeap {
	// Loop through the whole set of files to build the chunk heap.
	var ch chunkHeap
	id := r.mu.Lock()
	for _, file := range r.files {
		unfinishedChunks := r.buildUnfinishedChunks(file, hosts, fcidToHPK)
		ch = append(ch, unfinishedChunks...)
	}
	r.mu.Unlock(id)

	// Init the heap.
	heap.Init(&ch)
	return &ch
}

// managedInsertFileIntoChunkHeap will insert all of the chunks of a file into the
// chunk heap.
func (r *Renter) managedInsertFileIntoChunkHeap(f *file, ch *chunkHeap, hosts map[string]struct{}, fcidToHPK map[types.FileContractID]types.SiaPublicKey) {
	id := r.mu.Lock()
	unfinishedChunks := r.buildUnfinishedChunks(f, hosts, fcidToHPK)
	for i := 0; i < len(unfinishedChunks); i++ {
		heap.Push(ch, unfinishedChunks)
	}
	r.mu.Unlock(id)
}

// managedPrepareNextChunk takes the next chunk from the chunk heap and prepares
// it for upload. Preparation includes blocking until enough memory is
// available, fetching the logical data for the chunk (either from the disk or
// from the network), erasure coding the logical data into the physical data,
// and then finally passing the work onto the workers.
func (r *Renter) managedPrepareNextChunk(ch *chunkHeap, hosts map[string]struct{}, fcidToHPK map[types.FileContractID]types.SiaPublicKey) {
	// Grab the next chunk, loop until we have enough memory, update the amount
	// of memory available, and then spin up a thread to asynchronously handle
	// the rest of the chunk tasks.
	memoryAvailable := r.managedMemoryAvailableGet()
	nextChunk := ch.Pop().(*unfinishedChunk)
	for nextChunk.memoryNeeded > memoryAvailable {
		select {
		case newFile := <-r.newUploads:
			r.managedInsertFileIntoChunkHeap(newFile, ch, hosts, fcidToHPK)
		case <-r.newMemory:
		case <-r.tg.StopChan():
		}
	}
	r.managedMemoryAvailableSub(nextChunk.memoryNeeded)
	go r.managedFetchAndRepairChunk(nextChunk)
}

// threadedRepairScan is a background thread that checks on the health of files,
// tracking the least healthy files and queuing the worst ones for repair.
//
// TODO / NOTE: Once we have upgraded the filesystem, we can replace this with
// the tree-diving technique discussed in Sprint 5. For now we just iterate
// through all of our in-memory files and chunks, and maintain a finite list of
// the worst ones, and then iterate through it again when we need to find more
// things to repair.
func (r *Renter) threadedRepairScan() {
	err := r.tg.Add()
	if err != nil {
		return
	}
	defer r.tg.Done()

	for {
		// Return if the renter has shut down.
		select{
		case <-r.tg.StopChan():
			return
		default:
		}

		// Grab the current set of contracts and a lookup table from file
		// contract ids to host public keys. The lookup table has a mapping from
		// all historic file contract ids, which is necessary when using the
		// renter to perform downloads.
		//
		// TODO / NOTE: This code can be removed once files store the HostPubKey
		// of the hosts they are using, instead of just the FileContractID.
		currentContracts, fcidToHPK := r.managedCurrentContractsAndHistoricFCIDLookup()

		// Pull together a list of hosts that are available for uploading. We
		// assemble them into a map where the key is the String() representation
		// of a types.SiaPublicKey (which cannot be used as a map key itself).
		hosts := make(map[string]struct{})
		for _, contract := range currentContracts {
			hosts[contract.HostPublicKey.String()] = struct{}{}
		}

		// Build a min-heap of chunks organized by upload progress.
		chunkHeap := r.managedBuildChunkHeap(hosts, fcidToHPK)

		// Refresh the worker pool before beginning uploads.
		r.managedUpdateWorkerPool()

		// Work through the heap. Chunks will be processed one at a time until
		// the heap is whittled down. When the heap is empty, we wait for new
		// files in a loop and then process those. When the rebuild signal is
		// received, we start over with the outer loop that rebuilds the heap
		// and re-checks the health of all the files.
		rebuildHeapSignal := time.After(rebuildChunkHeapInterval)
		for {
			// Return if the renter has shut down.
			select{
			case <-r.tg.StopChan():
				return
			default:
			}

			if chunkHeap.Len() > 0 {
				r.managedPrepareNextChunk(chunkHeap, hosts, fcidToHPK)
			} else {
				// Block until the rebuild signal is received.
				select {
				case newFile := <-r.newUploads:
					// If a new file is received, add its chunks to the repair
					// heap and loop to start working through those chunks.
					r.managedInsertFileIntoChunkHeap(newFile, chunkHeap, hosts, fcidToHPK)
					continue
				case <-rebuildHeapSignal:
					// If the rebuild heap signal is received, break out to the
					// outer loop which will check the health of all filess
					// again and then rebuild the heap.
					break
				case <-r.tg.StopChan():
					// If the stop signal is received, quit entirely.
					return
				}
			}
		}
	}
}