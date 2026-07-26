package sessionstore

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func linkPath(store *Store, sessionID, responseID string) string {
	return filepath.Join(store.sessionLinksDir(safeName(sessionID)), safeName(responseID))
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s: %s still exists (%v)", why, path, err)
	}
}

func mustExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s: %s is missing (%v)", why, path, err)
	}
}

// TestSaveResponseMovesTheRetentionLinkWhenTheSessionChanges pins the invariant
// that makes the previous record's sdk_session_id worth reading at all: exactly
// the sessions a live response still points at may keep a retention link.
// Leaving the old link behind protects a session nothing references any more;
// dropping the new one lets retention delete a session a live response needs.
func TestSaveResponseMovesTheRetentionLinkWhenTheSessionChanges(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := ResponseRecord{ID: "resp_moved", SDKSessionID: "sdk_first", Stored: true, OutputText: strings.Repeat("x", 4096)}
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	mustExist(t, linkPath(store, "sdk_first", record.ID), "first save")

	record.SDKSessionID = "sdk_second"
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	mustExist(t, linkPath(store, "sdk_second", record.ID), "session change")
	mustNotExist(t, linkPath(store, "sdk_first", record.ID), "session change left a stale reference")

	// Re-saving under the same session must be idempotent, not a self-removal.
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	mustExist(t, linkPath(store, "sdk_second", record.ID), "idempotent re-save")

	// And deleting still retracts the surviving reference.
	if err := store.DeleteResponse(record.ID); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, linkPath(store, "sdk_second", record.ID), "delete left a reference behind")
}

// TestSaveResponseRefusesToOverwriteAnUnreadableRecord pins the classification
// SaveResponse applies to whatever is already on disk. A record it cannot make
// sense of is not silently replaced, and the distinction between "absent"
// (proceed) and "unreadable" (refuse) is what keeps a future-version record
// written by a newer binary from being clobbered by an older one.
func TestSaveResponseRefusesToOverwriteAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		onDisk  string
		wantErr func(error) bool
	}{
		{
			name:    "future version",
			onDisk:  `{"version":999,"id":"resp_x","sdk_session_id":"sdk","deleted":false,"stored":true}`,
			wantErr: func(err error) bool { var v *UnsupportedRecordVersionError; return errors.As(err, &v) },
		},
		{
			name:    "unversioned without id",
			onDisk:  `{"sdk_session_id":"sdk","deleted":false,"stored":true}`,
			wantErr: func(err error) bool { return err != nil && strings.Contains(err.Error(), "missing id") },
		},
		{
			name:    "truncated object",
			onDisk:  `{"version":3,"id":"resp_x","sdk_session_id":`,
			wantErr: func(err error) bool { return err != nil && !errors.Is(err, ErrNotFound) },
		},
		{
			name:    "not an object",
			onDisk:  `[]`,
			wantErr: func(err error) bool { return err != nil && !errors.Is(err, ErrNotFound) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := New(t.TempDir(), t.TempDir())
			if err := store.Ensure(); err != nil {
				t.Fatal(err)
			}
			path := store.responsePath("resp_x")
			if err := os.WriteFile(path, []byte(tc.onDisk), 0o600); err != nil {
				t.Fatal(err)
			}
			err := store.SaveResponse(ResponseRecord{ID: "resp_x", SDKSessionID: "sdk_new", Stored: true})
			if !tc.wantErr(err) {
				t.Fatalf("SaveResponse over %s = %v, want a refusal", tc.name, err)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != tc.onDisk {
				t.Fatalf("SaveResponse replaced an unreadable record:\n got  %s\n want %s", got, tc.onDisk)
			}
		})
	}
}

// TestSaveResponseHeaderAgreesWithTheFullRecord is the equivalence proof for
// reading only the header: for every record shape, the streamed header must
// report the same tombstone flag, the same session id, and the same error
// classification as decoding the whole record did.
func TestSaveResponseHeaderAgreesWithTheFullRecord(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 1<<20)
	records := []ResponseRecord{
		{Version: 3, ID: "a", SDKSessionID: "sdk_a", Stored: true},
		{Version: 3, ID: "b", SDKSessionID: "", Stored: true},
		{Version: 3, ID: "c", SDKSessionID: "sdk_c", Deleted: true, Status: "deleted"},
		{Version: 3, ID: "d", SDKSessionID: "sdk_d", Stored: true, InputText: big, OutputText: big},
		{Version: 3, ID: "e", SDKSessionID: "sdk_\u00e9\u2603", Stored: true, Instructions: "be brief"},
		goldenResponseRecord(),
	}
	for _, record := range records {
		if err := writeJSON(store.responsePath(record.ID), record); err != nil {
			t.Fatal(err)
		}
		header, headerErr := store.readResponseHeader(record.ID)
		full, fullErr := store.loadResponseRecord(record.ID)
		if (headerErr == nil) != (fullErr == nil) {
			t.Fatalf("record %q: header error = %v, full-record error = %v", record.ID, headerErr, fullErr)
		}
		if headerErr != nil {
			continue
		}
		if header.deleted != full.Deleted || header.sessionID != full.SDKSessionID || header.id != full.ID {
			t.Fatalf("record %q: header = %#v, full record = (id %q, session %q, deleted %v)", record.ID, header, full.ID, full.SDKSessionID, full.Deleted)
		}
	}

	// Hand-written shapes the store itself never emits, including a record whose
	// wanted fields appear after a large one and one that omits them entirely.
	for _, tc := range []struct {
		name   string
		onDisk string
	}{
		{"reordered after a large field", `{"version":3,"output_text":"` + big + `","id":"z","sdk_session_id":"sdk_z","deleted":true}`},
		{"missing deleted and session", `{"version":3,"id":"z","output":null,"output_text":""}`},
		{"unversioned with id", `{"id":"z","sdk_session_id":"sdk_z","deleted":false,"stored":true}`},
		{"nested objects before the fields", `{"version":3,"id":"z","usage":{"a":{"b":[1,2,{"c":null}]}},"sdk_session_id":"sdk_z","deleted":false}`},
	} {
		// Not parallel: every case writes and reads the same "z" record path.
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(store.responsePath("z"), []byte(tc.onDisk), 0o600); err != nil {
				t.Fatal(err)
			}
			header, headerErr := store.readResponseHeader("z")
			full, fullErr := store.loadResponseRecord("z")
			if (headerErr == nil) != (fullErr == nil) {
				t.Fatalf("header error = %v, full-record error = %v", headerErr, fullErr)
			}
			if headerErr != nil {
				return
			}
			if header.deleted != full.Deleted || header.sessionID != full.SDKSessionID {
				t.Fatalf("header = %#v, full record = (session %q, deleted %v)", header, full.SDKSessionID, full.Deleted)
			}
		})
	}
}

// TestSaveResponseHeaderResolvesDuplicateKeysFirstWins documents the one place
// the streamed header does not match a full json.Unmarshal.
//
// encoding/json resolves duplicate object names last-wins; stopping as soon as
// all four wanted fields have been seen necessarily takes the first. RFC 8259
// leaves duplicate names undefined, and a record this store wrote cannot
// contain them: writeJSON marshals a struct, which emits each key once. The
// divergence is therefore unreachable, but it is pinned rather than left to be
// rediscovered.
func TestSaveResponseHeaderResolvesDuplicateKeysFirstWins(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	const onDisk = `{"version":3,"id":"z","sdk_session_id":"sdk_first","deleted":false,"sdk_session_id":"sdk_second"}`
	if err := os.WriteFile(store.responsePath("z"), []byte(onDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	header, err := store.readResponseHeader("z")
	if err != nil {
		t.Fatal(err)
	}
	if header.sessionID != "sdk_first" {
		t.Fatalf("duplicate key resolved to %q, want the first occurrence", header.sessionID)
	}
}

// change: the cost of deciding whether to overwrite a record must not scale
// with the record's size. A 16 MiB record is read in a bounded number of bytes.
func TestSaveResponseHeaderStopsBeforeThePayload(t *testing.T) {
	t.Parallel()
	store := New(t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := ResponseRecord{Version: 3, ID: "resp_big", SDKSessionID: "sdk_big", Stored: true, OutputText: strings.Repeat("x", 16<<20)}
	if err := writeJSON(store.responsePath(record.ID), record); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(store.responsePath(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	counter := &countingReader{r: file}
	header, err := decodeResponseHeader(counter)
	if err != nil {
		t.Fatal(err)
	}
	if header.sessionID != "sdk_big" || header.deleted {
		t.Fatalf("header = %#v", header)
	}
	if counter.n > 64<<10 {
		t.Fatalf("reading the header consumed %d bytes of a %d byte record; it must stop before the payload", counter.n, len(encoded))
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestSaveResponseTombstoneSurvivesAProcessRestart checks the tombstone is
// still answered from disk, not only from the in-memory deletedIDs set, which
// is populated only while a pin is held.
func TestSaveResponseTombstoneSurvivesAProcessRestart(t *testing.T) {
	t.Parallel()
	dataDir, stateDir := t.TempDir(), t.TempDir()
	store := New(dataDir, stateDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := ResponseRecord{ID: "resp_gone", SDKSessionID: "sdk", Stored: true, OutputText: "answer"}
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteResponse(record.ID); err != nil {
		t.Fatal(err)
	}

	restarted := New(dataDir, stateDir)
	if err := restarted.Ensure(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.deletedIDs) != 0 {
		t.Fatalf("a fresh store started with in-memory tombstones: %#v", restarted.deletedIDs)
	}
	if err := restarted.SaveResponse(record); !errors.Is(err, ErrNotFound) {
		t.Fatalf("save over a tombstone after restart = %v, want ErrNotFound", err)
	}
	stored, err := restarted.loadResponseRecord(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Deleted || stored.OutputText != "" {
		t.Fatalf("tombstone was resurrected across a restart: %#v", stored)
	}
	mustNotExist(t, linkPath(restarted, "sdk", record.ID), "a resurrected save republished the retention link")
}
