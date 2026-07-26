package sessionstore

import (
	"os"
	"testing"
)

// TestSaveResponseHeaderRejectsATruncatedRecord covers the case a streamed
// header decode gets wrong if it trusts json.Decoder.More.
//
// More is `c, err := dec.peek(); return err == nil && c != ']' && c != '}'`, so
// it reports "no more values" identically for a document that closed and one
// whose reader died mid-way. A truncated record therefore used to decode as a
// complete header carrying whatever prefix happened to survive, with a nil
// error. That is not a cosmetic divergence from loadResponseRecord: a tombstone
// truncated ahead of its "deleted" key read as deleted:false, so SaveResponse's
// tombstone guard passed and a response the client had deleted came back.
//
// The store cannot itself produce a torn file - writeJSON is temp, fsync,
// rename, dirsync - but partial restores, filesystem damage and hand-edited
// records all reach this path, and surviving exactly those is the reason
// SaveResponse refuses to overwrite a record it cannot read.
func TestSaveResponseHeaderRejectsATruncatedRecord(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		onDisk string
	}{
		// Truncated ahead of "deleted": the header used to report deleted:false.
		{"tombstone truncated before deleted", `{"version":3,"id":"resp_t","sdk_session_id":"sdk_t","status":"deleted","stored":false`},
		// Truncated ahead of "sdk_session_id": previousSession used to read as "",
		// so the stale retention link was never retracted.
		{"truncated before the session id", `{"version":3,"id":"resp_t","status":"completed"`},
		{"truncated mid-value", `{"version":3,"id":"resp_t","sdk_session_id":`},
		{"truncated after a complete pair", `{"version":3,"id":"resp_t"`},
		{"empty file", ``},
	} {
		// Not parallel: every case writes and reads the same record path.
		t.Run(tc.name, func(t *testing.T) {
			path := store.responsePath("resp_t")
			if err := os.WriteFile(path, []byte(tc.onDisk), 0o600); err != nil {
				t.Fatal(err)
			}
			_, headerErr := store.readResponseHeader("resp_t")
			if _, fullErr := store.loadResponseRecord("resp_t"); fullErr == nil {
				t.Fatalf("precondition: loadResponseRecord accepted %q", tc.onDisk)
			}
			if headerErr == nil {
				t.Fatalf("readResponseHeader accepted a truncated record: %q", tc.onDisk)
			}

			// The consequence, in the store's own terms: a save must not overwrite
			// a record it could not read, nor resurrect a tombstone it could not
			// prove was absent.
			if err := store.SaveResponse(ResponseRecord{ID: "resp_t", SDKSessionID: "sdk_new", Stored: true}); err == nil {
				t.Fatal("SaveResponse overwrote a record whose previous contents were unreadable")
			}
			onDisk, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(onDisk) != tc.onDisk {
				t.Fatalf("bytes on disk changed: got %q, want %q", onDisk, tc.onDisk)
			}
		})
	}

	// An intact tombstone still reads as deleted, so the guard above rejects
	// truncation rather than rejecting everything.
	tombstone := `{"version":3,"id":"resp_t","sdk_session_id":"sdk_t","status":"deleted","stored":false,"deleted":true}`
	if err := os.WriteFile(store.responsePath("resp_t"), []byte(tombstone), 0o600); err != nil {
		t.Fatal(err)
	}
	header, err := store.readResponseHeader("resp_t")
	if err != nil {
		t.Fatal(err)
	}
	if !header.deleted {
		t.Fatalf("intact tombstone read as deleted=%v", header.deleted)
	}
}
