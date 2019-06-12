package gc

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/pachyderm/pachyderm/src/client/pkg/require"
	"github.com/pachyderm/pachyderm/src/server/pkg/storage/chunk"
	"github.com/pachyderm/pachyderm/src/server/pkg/testutil"
	"golang.org/x/sync/errgroup"
)

var _ = fmt.Printf // TODO: remove after debugging is done
var _ = sql.OpenDB

// Dummy deleter object for testing, so we don't need an object storage
type testDeleter struct{}

func (td *testDeleter) Delete(ctx context.Context, chunks []chunk.Chunk) error {
	return nil
}

func makeServer(t *testing.T) Server {
	server, err := MakeServer(&testDeleter{}, "localhost", 32228)
	require.NoError(t, err)
	return server
}

func makeClient(t *testing.T, ctx context.Context, server Server) *ClientImpl {
	if server == nil {
		server = makeServer(t)
	}
	gcc, err := MakeClient(ctx, server, "localhost", 32228)
	require.NoError(t, err)
	return gcc.(*ClientImpl)
}

func initialize(t *testing.T, ctx context.Context) *ClientImpl {
	gcc := makeClient(t, ctx, nil)
	_, err := gcc.db.QueryContext(ctx, "delete from chunks *; delete from refs *;")
	require.NoError(t, err)
	return gcc
}

type chunkRow struct {
	chunk    string
	deleting bool
}

func allChunkRows(t *testing.T, ctx context.Context, gcc *ClientImpl) []chunkRow {
	rows, err := gcc.db.QueryContext(ctx, "select chunk, deleting from chunks")
	require.NoError(t, err)

	defer rows.Close()

	chunks := []chunkRow{}

	for rows.Next() {
		var chunk string
		var timestamp pq.NullTime
		require.NoError(t, rows.Scan(&chunk, &timestamp))
		chunks = append(chunks, chunkRow{chunk: chunk, deleting: timestamp.Valid})
	}
	require.NoError(t, rows.Err())

	return chunks
}

type refRow struct {
	sourcetype string
	source     string
	chunk      string
}

func allRefRows(t *testing.T, ctx context.Context, gcc *ClientImpl) []refRow {
	rows, err := gcc.db.QueryContext(ctx, "select sourcetype, source, chunk from refs")
	require.NoError(t, err)
	defer rows.Close()

	refs := []refRow{}

	for rows.Next() {
		row := refRow{}
		require.NoError(t, rows.Scan(&row.sourcetype, &row.source, &row.chunk))
		refs = append(refs, row)
	}
	require.NoError(t, rows.Err())

	return refs
}

func printState(t *testing.T, ctx context.Context, gcc *ClientImpl) {
	chunkRows := allChunkRows(t, ctx, gcc)
	fmt.Printf("Chunks table:\n")
	for _, row := range chunkRows {
		fmt.Printf("  %v\n", row)
	}

	refRows := allRefRows(t, ctx, gcc)
	fmt.Printf("Refs table:\n")
	for _, row := range refRows {
		fmt.Printf("  %v\n", row)
	}
}

func makeJobs(count int) []string {
	result := []string{}
	for i := 0; i < count; i++ {
		result = append(result, fmt.Sprintf("job-%d", i))
	}
	return result
}

func makeChunks(count int) []chunk.Chunk {
	result := []chunk.Chunk{}
	for i := 0; i < count; i++ {
		result = append(result, chunk.Chunk{Hash: testutil.UniqueString(fmt.Sprintf("chunk-%d-", i))})
	}
	return result
}

func TestConnectivity(t *testing.T) {
	initialize(t, context.Background())
}

func TestReserveChunks(t *testing.T) {
	ctx := context.Background()
	gcc := initialize(t, ctx)
	jobs := makeJobs(3)
	chunks := makeChunks(3)

	// No chunks
	require.NoError(t, gcc.ReserveChunks(ctx, jobs[0], chunks[0:0]))

	// One chunk
	require.NoError(t, gcc.ReserveChunks(ctx, jobs[1], chunks[0:1]))

	// Multiple chunks
	require.NoError(t, gcc.ReserveChunks(ctx, jobs[2], chunks))

	expectedChunkRows := []chunkRow{
		{chunks[0].Hash, false},
		{chunks[1].Hash, false},
		{chunks[2].Hash, false},
	}
	require.ElementsEqual(t, expectedChunkRows, allChunkRows(t, ctx, gcc))

	expectedRefRows := []refRow{
		{"job", jobs[1], chunks[0].Hash},
		{"job", jobs[2], chunks[0].Hash},
		{"job", jobs[2], chunks[1].Hash},
		{"job", jobs[2], chunks[2].Hash},
	}
	require.ElementsEqual(t, expectedRefRows, allRefRows(t, ctx, gcc))
}

func TestUpdateReferences(t *testing.T) {
	ctx := context.Background()
	gcc := initialize(t, ctx)
	jobs := makeJobs(3)
	chunks := makeChunks(5)

	require.NoError(t, gcc.ReserveChunks(ctx, jobs[0], chunks[0:3])) // 0, 1, 2
	require.NoError(t, gcc.ReserveChunks(ctx, jobs[1], chunks[2:4])) // 2, 3
	require.NoError(t, gcc.ReserveChunks(ctx, jobs[2], chunks[4:5])) // 4

	// Currently, no links between chunks:
	// 0 1 2 3 4
	expectedChunkRows := []chunkRow{
		{chunks[0].Hash, false},
		{chunks[1].Hash, false},
		{chunks[2].Hash, false},
		{chunks[3].Hash, false},
		{chunks[4].Hash, false},
	}
	require.ElementsEqual(t, expectedChunkRows, allChunkRows(t, ctx, gcc))

	expectedRefRows := []refRow{
		{"job", jobs[0], chunks[0].Hash},
		{"job", jobs[0], chunks[1].Hash},
		{"job", jobs[0], chunks[2].Hash},
		{"job", jobs[1], chunks[2].Hash},
		{"job", jobs[1], chunks[3].Hash},
		{"job", jobs[2], chunks[4].Hash},
	}
	require.ElementsEqual(t, expectedRefRows, allRefRows(t, ctx, gcc))

	require.NoError(t, gcc.UpdateReferences(
		ctx,
		[]Reference{{"chunk", chunks[4].Hash, chunks[0]}},
		[]Reference{},
		jobs[0],
	))

	// Chunk 1 should be cleaned up as unreferenced
	// 4 2 3 <- referenced by jobs
	// |
	// 0
	expectedChunkRows = []chunkRow{
		{chunks[0].Hash, false},
		{chunks[1].Hash, true},
		{chunks[2].Hash, false},
		{chunks[3].Hash, false},
		{chunks[4].Hash, false},
	}
	require.ElementsEqual(t, expectedChunkRows, allChunkRows(t, ctx, gcc))

	expectedRefRows = []refRow{
		{"job", jobs[1], chunks[2].Hash},
		{"job", jobs[1], chunks[3].Hash},
		{"job", jobs[2], chunks[4].Hash},
		{"chunk", chunks[4].Hash, chunks[0].Hash},
	}
	require.ElementsEqual(t, expectedRefRows, allRefRows(t, ctx, gcc))

	require.NoError(t, gcc.UpdateReferences(
		ctx,
		[]Reference{
			{"semantic", "semantic-3", chunks[3]},
			{"semantic", "semantic-2", chunks[2]},
		},
		[]Reference{},
		jobs[1],
	))

	// No chunks should be cleaned up, the job reference to 2 and 3 were replaced
	// with semantic references
	// 2 3 <- referenced semantically
	// 4 <- referenced by jobs
	// |
	// 0
	expectedChunkRows = []chunkRow{
		{chunks[0].Hash, false},
		{chunks[1].Hash, true},
		{chunks[2].Hash, false},
		{chunks[3].Hash, false},
		{chunks[4].Hash, false},
	}
	require.ElementsEqual(t, expectedChunkRows, allChunkRows(t, ctx, gcc))

	expectedRefRows = []refRow{
		{"job", jobs[2], chunks[4].Hash},
		{"chunk", chunks[4].Hash, chunks[0].Hash},
		{"semantic", "semantic-2", chunks[2].Hash},
		{"semantic", "semantic-3", chunks[3].Hash},
	}
	require.ElementsEqual(t, expectedRefRows, allRefRows(t, ctx, gcc))

	require.NoError(t, gcc.UpdateReferences(
		ctx,
		[]Reference{{"chunk", chunks[4].Hash, chunks[2]}},
		[]Reference{{"semantic", "semantic-3", chunks[3]}},
		jobs[2],
	))

	// Chunk 3 should be cleaned up as the semantic reference was removed
	// Chunk 4 should be cleaned up as the job reference was removed
	// Chunk 0 should be cleaned up later once chunk 4 has been removed
	// 2 <- referenced semantically
	// 0 <- referenced by 4 (deleting)
	expectedChunkRows = []chunkRow{
		{chunks[0].Hash, false},
		{chunks[1].Hash, true},
		{chunks[2].Hash, false},
		{chunks[3].Hash, true},
		{chunks[4].Hash, true},
	}
	require.ElementsEqual(t, expectedChunkRows, allChunkRows(t, ctx, gcc))

	expectedRefRows = []refRow{
		{"chunk", chunks[4].Hash, chunks[0].Hash},
		{"chunk", chunks[4].Hash, chunks[2].Hash},
		{"semantic", "semantic-2", chunks[2].Hash},
	}
	require.ElementsEqual(t, expectedRefRows, allRefRows(t, ctx, gcc))
}

func TestFuzz(t *testing.T) {
	type jobData struct {
		id     string
		add    []Reference
		remove []Reference
	}

	runJob := func(ctx context.Context, gcc Client, job jobData) error {
		// Make a list of chunks we'll be referencing and reserve them
		chunkMap := map[string]bool{}
		for _, item := range job.add {
			chunkMap[item.chunk.Hash] = true
			if item.sourcetype == "chunk" {
				chunkMap[item.source] = true
			}
		}
		reservedChunks := []chunk.Chunk{}
		for hash := range chunkMap {
			reservedChunks = append(reservedChunks, chunk.Chunk{Hash: hash})
		}

		fmt.Printf("client job (%s) reserving %d chunks: %v\n", job.id, len(reservedChunks), reservedChunks)
		if err := gcc.ReserveChunks(ctx, job.id, reservedChunks); err != nil {
			return err
		}

		// Pretend we write to object storage here
		fmt.Printf("client job (%s) sleeping\n", job.id)
		time.Sleep(time.Duration(rand.Float32()*50) * time.Millisecond)

		fmt.Printf("client job (%s) updating references, %d add, %d remove\n", job.id, len(job.add), len(job.remove))
		return gcc.UpdateReferences(ctx, job.add, job.remove, job.id)
	}

	server := makeServer(t)
	startClients := func(numClients int, jobChannel chan jobData) *errgroup.Group {
		eg, ctx := errgroup.WithContext(context.Background())
		for i := 0; i < numClients; i++ {
			eg.Go(func() error {
				gcc := makeClient(t, ctx, server)
				for x := range jobChannel {
					if err := runJob(ctx, gcc, x); err != nil {
						fmt.Printf("client failed: %v\n", err)
						return err
					}
				}
				fmt.Printf("client graceful exit")
				return nil
			})
		}
		return eg
	}

	// Global ref registry to track what the database should look like
	chunkRefs := map[int][]Reference{}
	freeChunkIds := []int{}
	maxId := -1

	usedChunkId := func(lowerBound int) int {
		ids := []int{}
		// TODO: this might get annoyingly slow, but go doesn't have good sorted data structures
		for id := range chunkRefs {
			if id >= lowerBound {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return 0
		}
		return ids[rand.Intn(len(ids))]
	}

	unusedChunkId := func(lowerBound int) int {
		for i, id := range freeChunkIds {
			if id >= lowerBound {
				freeChunkIds = append(freeChunkIds[0:i], freeChunkIds[i+1:len(freeChunkIds)]...)
				return id
			}
		}
		maxId++
		return maxId
	}

	addRef := func() Reference {
		var dest int
		ref := Reference{}
		roll := rand.Float32()
		if len(chunkRefs) == 0 || roll > 0.9 {
			// Make a new chunk with a semantic reference
			dest = unusedChunkId(0)
			ref.sourcetype = "semantic"
			ref.source = testutil.UniqueString("semantic-")
			ref.chunk = chunk.Chunk{Hash: fmt.Sprintf("chunk-%d", dest)}
		} else if roll > 0.6 {
			// Add a semantic reference to an existing chunk
			dest = usedChunkId(0)
			ref.sourcetype = "semantic"
			ref.source = testutil.UniqueString("semantic-")
			ref.chunk = chunk.Chunk{Hash: fmt.Sprintf("chunk-%d", dest)}
		} else {
			source := usedChunkId(0)
			ref.sourcetype = "chunk"
			ref.source = fmt.Sprintf("chunk-%d", source)
			if roll > 0.5 {
				// Add a cross-chunk reference to a new chunk
				dest = unusedChunkId(source + 1)
				ref.chunk = chunk.Chunk{Hash: fmt.Sprintf("chunk-%d", dest)}
			} else {
				// Add a cross-chunk reference to an existing chunk
				dest = usedChunkId(source + 1)
				if dest == 0 {
					dest = unusedChunkId(source + 1)
				}
				ref.chunk = chunk.Chunk{Hash: fmt.Sprintf("chunk-%d", dest)}
			}
		}

		existingChunkRefs := chunkRefs[dest]
		if existingChunkRefs == nil {
			chunkRefs[dest] = []Reference{ref}
		} else {
			// Make sure ref isn't a duplicate
			for _, x := range existingChunkRefs {
				if ref.sourcetype == x.sourcetype && ref.source == x.source {
					ref.sourcetype = "semantic"
					ref.source = testutil.UniqueString("semantic-")
					break
				}
			}

			chunkRefs[dest] = append(existingChunkRefs, ref)
		}

		return ref
	}

	removeRef := func() Reference {
		if len(chunkRefs) == 0 {
			panic("no references to remove")
		}

		i := rand.Intn(len(chunkRefs))
		for k, v := range chunkRefs {
			if i == 0 {
				j := rand.Intn(len(v))
				ref := v[j]
				chunkRefs[k] = append(v[0:j], v[j+1:len(v)]...)
				if len(chunkRefs[k]) == 0 {
					delete(chunkRefs, k)
					freeChunkIds = append(freeChunkIds, k)
				}
				return ref
			}
			i--
		}
		panic("unreachable")
	}

	makeJobData := func() jobData {
		jd := jobData{id: testutil.UniqueString("job-")}

		// Run random add/remove operations, only allow references to go from lower numbers to higher numbers to keep it acyclic
		for rand.Float32() > 0.5 {
			numAdds := rand.Intn(10)
			for i := 0; i < numAdds; i++ {
				jd.add = append(jd.add, addRef())
			}
		}

		for rand.Float32() > 0.6 {
			numRemoves := rand.Intn(7)
			for i := 0; i < numRemoves && len(chunkRefs) > 0; i++ {
				jd.remove = append(jd.remove, removeRef())
			}
		}

		return jd
	}

	verifyData := func() {
		fmt.Printf("verifyData\n")
	}

	numClients := 3
	numJobs := 1000
	// Occasionally halt all goroutines and check data consistency
	for i := 0; i < 5; i++ {
		jobChan := make(chan jobData, numJobs)
		eg := startClients(numClients, jobChan)
		for i := 0; i < numJobs; i++ {
			jobChan <- makeJobData()
		}
		close(jobChan)
		require.NoError(t, eg.Wait())

		verifyData()
	}
}
