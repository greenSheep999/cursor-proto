package kernel

import "testing"

// TestTeamIDFromIDEValuesHandlesBothIDEShapes pins the parser's contract
// against the two cursorAuth/* keys we've seen in Cursor IDE's state.vscdb:
//
//   - cursorAuth/teamId — a bare decimal string when the account belongs to
//     a team, absent otherwise. Some builds intentionally write "0" here on
//     personal accounts; treat that the same as absent.
//   - cursorAuth/cachedTeam — a JSON blob storageService.getObject persists
//     for the UI: `{"teamId":<n>,"name":"..."}`. Older IDE builds
//     double-encode by writing the JSON as a JSON string, so the parser
//     must accept both `{"teamId":42,…}` and `"{\"teamId\":42,…}"`.
//
// Personal-account snapshots (no team keys, or a stub cachedTeam with
// teamId=0) must return "" so ApplyCommonHeaders does not emit a stale
// x-cursor-team-id header.
func TestTeamIDFromIDEValuesHandlesBothIDEShapes(t *testing.T) {
	cases := []struct {
		name   string
		direct string
		cached string
		want   string
	}{
		{name: "personal-no-keys", direct: "", cached: "", want: ""},
		{name: "personal-zero-direct", direct: "0", cached: "", want: ""},
		{name: "team-direct-only", direct: "1234567", cached: "", want: "1234567"},
		{name: "team-cached-json", direct: "", cached: `{"teamId":42,"name":"Acme"}`, want: "42"},
		{name: "team-cached-string", direct: "", cached: `"{\"teamId\":42,\"name\":\"Acme\"}"`, want: "42"},
		{name: "team-cached-team-id-zero", direct: "", cached: `{"teamId":0,"name":""}`, want: ""},
		{name: "team-cached-team-id-string", direct: "", cached: `{"teamId":"9001","name":"Acme"}`, want: "9001"},
		{name: "direct-wins-over-cached", direct: "1", cached: `{"teamId":42}`, want: "1"},
		{name: "malformed-cached", direct: "", cached: `not-json`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := teamIDFromIDEValues(tc.direct, tc.cached); got != tc.want {
				t.Errorf("teamIDFromIDEValues(%q, %q) = %q, want %q", tc.direct, tc.cached, got, tc.want)
			}
		})
	}
}
