package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/cockroachdb/pebble"
)

// vaultScopedExportPrefixes lists every prefix scoped by a vault workspace
// that should be exported. 0x01 (engrams) is iterated first with the engram
// count tracked. 0x0E (VaultMetaKey) and 0x0F (VaultNameIndexKey) are
// global/name keys excluded from the data stream (written by WriteVaultName
// on import). 0x11 (DigestFlagsKey) is globally keyed by ULID — excluded.
var vaultScopedExportPrefixes = []byte{
	0x01, // engrams (full record)
	0x02, // metadata-only
	0x03, // forward associations
	0x04, // reverse associations
	0x05, // FTS posting lists
	0x06, // trigrams
	0x07, // HNSW node neighbors
	0x08, // FTS global stats
	0x09, // per-term FTS stats
	0x0A, // contradictions
	0x0B, // state secondary index
	0x0C, // tag secondary index
	0x0D, // creator secondary index
	0x10, // relevance bucket index
	0x12, // coherence counters
	0x13, // vault scoring weights
	0x14, // association weight index
	0x15, // vault count key + episode keys
	0x16, // provenance
	0x17, // bucket migration state
}

const exportBatchSize = 512

// ExportVaultData streams all vault-scoped keys to w as a gzip'd tar archive.
//
// The archive contains two entries:
//   - manifest.json: JSON-encoded MuninnManifest
//   - data.kvs: binary stream of (key_len uint32 BE)(key bytes)(val_len uint32 BE)(val bytes)
//     where the 8 workspace bytes are stripped from bytes 1-8 of every key.
//
// Returns an ExportResult with the engram count and total key count.
func (ps *PebbleStore) ExportVaultData(
	ctx context.Context,
	ws [8]byte,
	vaultName string,
	opts ExportOpts,
	w io.Writer,
) (*ExportResult, error) {
	wsNext, err := incrementWS(ws)
	if err != nil {
		return nil, fmt.Errorf("export: %w", err)
	}

	// Phase 1: buffer data.kvs in memory so we know the size for the tar header.
	var kvBuf bytes.Buffer
	var engramCount int64
	var totalKeys int64

	for _, p := range vaultScopedExportPrefixes {
		lo := make([]byte, 9)
		lo[0] = p
		copy(lo[1:], ws[:])
		hi := make([]byte, 9)
		hi[0] = p
		copy(hi[1:], wsNext[:])

		iter, iterErr := ps.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if iterErr != nil {
			return nil, fmt.Errorf("export: iter prefix 0x%02X: %w", p, iterErr)
		}

		for valid := iter.First(); valid; valid = iter.Next() {
			select {
			case <-ctx.Done():
				iter.Close()
				return nil, ctx.Err()
			default:
			}

			k := iter.Key()
			v := iter.Value()

			// Strip the 8 workspace bytes (positions 1-8) from the key.
			// Exported key: [prefix_byte][rest of key after ws bytes]
			stripped := make([]byte, 1+len(k)-9)
			stripped[0] = k[0]
			copy(stripped[1:], k[9:])

			rawVal := make([]byte, len(v))
			copy(rawVal, v)

			// Write (key_len uint32)(key)(val_len uint32)(val)
			var lenBuf [4]byte
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(stripped)))
			kvBuf.Write(lenBuf[:])
			kvBuf.Write(stripped)
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(rawVal)))
			kvBuf.Write(lenBuf[:])
			kvBuf.Write(rawVal)

			if p == 0x01 {
				engramCount++
			}
			totalKeys++
		}
		if err := iter.Close(); err != nil {
			return nil, fmt.Errorf("export: close iter prefix 0x%02X: %w", p, err)
		}
	}

	// Phase 2: build manifest.
	manifest := MuninnManifest{
		MuninnVersion: "1",
		SchemaVersion: MuninnSchemaVersion,
		Vault:         vaultName,
		EmbedderModel: opts.EmbedderModel,
		Dimension:     opts.Dimension,
		EngramCount:   engramCount,
		CreatedAt:     time.Now().UTC(),
		ResetMetadata: opts.ResetMetadata,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("export: marshal manifest: %w", err)
	}

	// Phase 3: write gzip'd tar archive.
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	// manifest.json entry
	if err := tw.WriteHeader(&tar.Header{
		Name:    "manifest.json",
		Mode:    0644,
		Size:    int64(len(manifestBytes)),
		ModTime: manifest.CreatedAt,
		Typeflag: tar.TypeReg,
	}); err != nil {
		return nil, fmt.Errorf("export: tar header manifest: %w", err)
	}
	if _, err := tw.Write(manifestBytes); err != nil {
		return nil, fmt.Errorf("export: tar write manifest: %w", err)
	}

	// data.kvs entry
	kvData := kvBuf.Bytes()
	if err := tw.WriteHeader(&tar.Header{
		Name:    "data.kvs",
		Mode:    0644,
		Size:    int64(len(kvData)),
		ModTime: manifest.CreatedAt,
		Typeflag: tar.TypeReg,
	}); err != nil {
		return nil, fmt.Errorf("export: tar header data.kvs: %w", err)
	}
	if _, err := tw.Write(kvData); err != nil {
		return nil, fmt.Errorf("export: tar write data.kvs: %w", err)
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("export: tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("export: gzip close: %w", err)
	}

	return &ExportResult{
		EngramCount: engramCount,
		TotalKeys:   totalKeys,
	}, nil
}

// ImportVaultData reads a .muninn gzip'd tar archive from r and writes all
// keys into wsTarget. The 8 workspace bytes stripped during export are
// re-inserted at positions 1-8 of each key using the target ws.
//
// ImportVaultData validates the manifest schema version and (optionally) the
// embedder model/dimension. It does NOT write the vault name index —
// the caller is responsible for calling WriteVaultName before importing.
func (ps *PebbleStore) ImportVaultData(
	ctx context.Context,
	wsTarget [8]byte,
	vaultName string,
	opts ImportOpts,
	r io.Reader,
) (*ExportResult, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("import: gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	var manifest MuninnManifest
	var kvData []byte
	gotManifest := false
	gotKV := false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("import: tar next: %w", err)
		}
		switch hdr.Name {
		case "manifest.json":
			if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
				return nil, fmt.Errorf("import: decode manifest: %w", err)
			}
			gotManifest = true
		case "data.kvs":
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("import: read data.kvs: %w", err)
			}
			kvData = data
			gotKV = true
		}
	}

	if !gotManifest {
		return nil, fmt.Errorf("import: archive missing manifest.json")
	}
	if !gotKV {
		// Empty vault — no data.kvs is acceptable if engramCount == 0.
		if manifest.EngramCount != 0 {
			return nil, fmt.Errorf("import: archive missing data.kvs but manifest shows %d engrams", manifest.EngramCount)
		}
		return &ExportResult{EngramCount: 0, TotalKeys: 0}, nil
	}

	if manifest.SchemaVersion != MuninnSchemaVersion && !opts.SkipCompatCheck {
		return nil, fmt.Errorf("import: schema version mismatch: archive=%d, current=%d",
			manifest.SchemaVersion, MuninnSchemaVersion)
	}
	if !opts.SkipCompatCheck && opts.ExpectedModel != "" && manifest.EmbedderModel != opts.ExpectedModel {
		return nil, fmt.Errorf("import: embedder model mismatch: archive=%q, expected=%q",
			manifest.EmbedderModel, opts.ExpectedModel)
	}
	if !opts.SkipCompatCheck && opts.ExpectedDimension != 0 && manifest.Dimension != opts.ExpectedDimension {
		return nil, fmt.Errorf("import: dimension mismatch: archive=%d, expected=%d",
			manifest.Dimension, opts.ExpectedDimension)
	}

	// Replay the KV stream into pebble.
	var engramCount int64
	var totalKeys int64

	rdr := bytes.NewReader(kvData)
	batch := ps.db.NewBatch()
	batchCount := 0

	var lenBuf [4]byte

	for {
		select {
		case <-ctx.Done():
			batch.Close()
			return nil, ctx.Err()
		default:
		}

		// Read key length.
		if _, err := io.ReadFull(rdr, lenBuf[:]); err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		} else if err != nil {
			batch.Close()
			return nil, fmt.Errorf("import: read key len: %w", err)
		}
		keyLen := binary.BigEndian.Uint32(lenBuf[:])

		strippedKey := make([]byte, keyLen)
		if _, err := io.ReadFull(rdr, strippedKey); err != nil {
			batch.Close()
			return nil, fmt.Errorf("import: read key: %w", err)
		}

		// Read value length.
		if _, err := io.ReadFull(rdr, lenBuf[:]); err != nil {
			batch.Close()
			return nil, fmt.Errorf("import: read val len: %w", err)
		}
		valLen := binary.BigEndian.Uint32(lenBuf[:])

		val := make([]byte, valLen)
		if _, err := io.ReadFull(rdr, val); err != nil {
			batch.Close()
			return nil, fmt.Errorf("import: read val: %w", err)
		}

		// Reconstruct full key: insert the 8 ws bytes at positions 1-8.
		// strippedKey = [prefix_byte][rest...], len >= 1
		fullKey := make([]byte, 1+8+len(strippedKey)-1)
		fullKey[0] = strippedKey[0]
		copy(fullKey[1:9], wsTarget[:])
		copy(fullKey[9:], strippedKey[1:])

		batch.Set(fullKey, val, nil)
		if strippedKey[0] == 0x01 {
			engramCount++
		}
		totalKeys++
		batchCount++

		if batchCount >= exportBatchSize {
			if err := batch.Commit(pebble.NoSync); err != nil {
				batch.Close()
				return nil, fmt.Errorf("import: commit batch: %w", err)
			}
			batch.Close()
			batch = ps.db.NewBatch()
			batchCount = 0
		}
	}

	if batchCount > 0 {
		if err := batch.Commit(pebble.NoSync); err != nil {
			batch.Close()
			return nil, fmt.Errorf("import: commit final batch: %w", err)
		}
	}
	batch.Close()

	// Seed the in-memory vault counter.
	vc := ps.getOrInitCounter(ctx, wsTarget)
	vc.count.Store(engramCount)

	return &ExportResult{
		EngramCount: engramCount,
		TotalKeys:   totalKeys,
	}, nil
}
