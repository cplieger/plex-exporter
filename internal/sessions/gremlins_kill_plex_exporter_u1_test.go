package sessions

import (
	"reflect"
	"testing"
)

// gk_plex_exporter_u1_snapshotIndex returns a copy of the tracker's
// transcodeIndex contents under the tracker lock, so tests can assert on the
// exact set of (transcodeKey -> sessionKey) entries written by
// UpdateLibraryLabels. Always non-nil so an empty result compares equal to a
// map[string]string{} literal under reflect.DeepEqual.
func gk_plex_exporter_u1_snapshotIndex(t *testing.T, tr *Tracker) map[string]string {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make(map[string]string, len(tr.transcodeIndex))
	for k, v := range tr.transcodeIndex {
		out[k] = v
	}
	return out
}

// TestGkPlexExporterU1_UpdateLibraryLabels_transcodeIndexWriteGate pins the
// two-operand guard
//
//	if ss.TranscodeKey != "" && ss.TranscodeKey != oldKey { ... write index ... }
//
// in UpdateLibraryLabels (tracker.go:153). It kills the two
// CONDITIONALS_NEGATION mutants there by asserting the exact transcodeIndex
// state after the call, across inputs where each operand flips the outcome:
//
//   - first operand  `!= ""`     (col 22): original requires the post-fn key
//     to be NON-empty to write. The "cleared key" case (post == "") proves the
//     index stays empty; a `== ""` mutant would instead write under the empty
//     key.
//   - second operand `!= oldKey` (col 47): original only writes when the key
//     actually CHANGED. The "unchanged key" case proves the index stays empty;
//     a `== oldKey` mutant would write a spurious entry.
//   - the "new non-empty key" case proves the entry IS written when both
//     operands are true; either negation drops it.
func TestGkPlexExporterU1_UpdateLibraryLabels_transcodeIndexWriteGate(t *testing.T) {
	const id = "sess-1"

	cases := []struct {
		name      string
		preKey    string            // ss.TranscodeKey before fn runs (oldKey)
		postKey   string            // ss.TranscodeKey fn sets
		wantIndex map[string]string // expected transcodeIndex after the call
	}{
		{
			// Non-empty AND changed from old: original writes the entry.
			// Mutant `== ""` (first operand) short-circuits false -> no write.
			// Mutant `== oldKey` (second operand) is false here -> no write.
			// So either negation makes the entry disappear.
			name:      "new non-empty key writes index entry",
			preKey:    "",
			postKey:   "tk-new",
			wantIndex: map[string]string{"tk-new": id},
		},
		{
			// Non-empty but UNCHANGED: original's second operand is false ->
			// no write. A `== oldKey` mutant flips it true -> spurious entry.
			name:      "unchanged non-empty key writes nothing",
			preKey:    "tk-x",
			postKey:   "tk-x",
			wantIndex: map[string]string{},
		},
		{
			// Cleared to empty: original's first operand is false -> no write.
			// A `== ""` mutant flips it true, and the second operand
			// (`"" != "tk-y"`) is true, so it writes an entry under the empty
			// key.
			name:      "cleared key writes nothing",
			preKey:    "tk-y",
			postKey:   "",
			wantIndex: map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			tr.mu.Lock()
			tr.Sessions[id] = Session{TranscodeKey: tc.preKey}
			tr.mu.Unlock()

			tr.UpdateLibraryLabels(id, func(s *Session) {
				s.TranscodeKey = tc.postKey
			})

			got := gk_plex_exporter_u1_snapshotIndex(t, tr)
			if !reflect.DeepEqual(got, tc.wantIndex) {
				t.Errorf("UpdateLibraryLabels(preKey=%q -> postKey=%q): transcodeIndex = %v, want %v",
					tc.preKey, tc.postKey, got, tc.wantIndex)
			}
		})
	}
}
