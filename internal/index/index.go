package index

// Entry represents the compact in-memory index pointer for a live key.
// Storing only the 8-byte file offset and 4-byte length ensures 1,000,000 keys
// consume minimal RAM (~32 MB overhead for entry structures).
type Entry struct {
	ValueOffset int64  // File offset where value bytes begin
	ValueLength uint32 // Length in bytes of the value payload
}

// Op represents a single in-flight operation to apply to the index upon transaction commit.
type Op struct {
	IsDelete    bool
	Key         string
	ValueOffset int64
	ValueLength uint32
}

// Index maintains the in-memory hash index mapping keys to on-disk value locations.
type Index struct {
	entries map[string]Entry
}

// New creates an empty hash index.
func New() *Index {
	return &Index{
		entries: make(map[string]Entry),
	}
}

// NewWithCapacity creates an index pre-sized for expected key counts.
func NewWithCapacity(capacity int) *Index {
	return &Index{
		entries: make(map[string]Entry, capacity),
	}
}

// Get looks up a key in the index.
func (idx *Index) Get(key string) (Entry, bool) {
	entry, ok := idx.entries[key]
	return entry, ok
}

// Put updates or inserts a key in the index.
func (idx *Index) Put(key string, entry Entry) {
	idx.entries[key] = entry
}

// Delete removes a key from the index.
func (idx *Index) Delete(key string) {
	delete(idx.entries, key)
}

// Len returns the current count of live keys in the database.
func (idx *Index) Len() int {
	return len(idx.entries)
}

// ApplyCommittedOps applies an ordered list of transaction operations to the index.
// Guaranteed batch semantics: operations are applied strictly in order, so the last
// operation on any given key determines its final presence and value.
func (idx *Index) ApplyCommittedOps(ops []Op) {
	for _, op := range ops {
		if op.IsDelete {
			delete(idx.entries, op.Key)
		} else {
			idx.entries[op.Key] = Entry{
				ValueOffset: op.ValueOffset,
				ValueLength: op.ValueLength,
			}
		}
	}
}

// Keys returns a slice of all live keys (used by dump and compaction).
func (idx *Index) Keys() []string {
	keys := make([]string, 0, len(idx.entries))
	for k := range idx.entries {
		keys = append(keys, k)
	}
	return keys
}
