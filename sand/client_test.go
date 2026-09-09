package sand

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/cursor-proto/auth"
)

func testAccount() *auth.Account {
	return &auth.Account{
		Email:       "test@example.com",
		UserID:      "user_test123",
		AccessToken: "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhdXRoMHx1c2VyX3Rlc3QxMjMifQ.sig",
	}
}

func TestCredentialCookieUsesEncodedCompoundToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); got != "WorkosCursorSessionToken=user_test123%3A%3AeyJhbGciOiJub25lIn0.eyJzdWIiOiJhdXRoMHx1c2VyX3Rlc3QxMjMifQ.sig" {
			t.Fatalf("cookie = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(testAccount(), WithBaseURL(server.URL))
	state, err := client.ProbeTokenAlive(context.Background())
	if err != nil || state != ProbeAlive {
		t.Fatalf("probe = %s, %v", state, err)
	}
}

func TestProbeTokenAliveClassifiesStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   ProbeState
		err    bool
	}{
		{name: "dead 401", status: http.StatusUnauthorized, want: ProbeDead},
		{name: "dead 403", status: http.StatusForbidden, want: ProbeDead},
		{name: "unknown 500", status: http.StatusInternalServerError, want: ProbeUnknown, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "bad")
			}))
			defer server.Close()
			state, err := New(testAccount(), WithBaseURL(server.URL)).ProbeTokenAlive(context.Background())
			if state != tc.want || (err != nil) != tc.err {
				t.Fatalf("probe = %s, %v; want %s, err=%v", state, err, tc.want, tc.err)
			}
		})
	}
}

func TestClaimOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		teamID     string
		usage      *Usage
		access     string
		trialBody  string
		want       Outcome
		wantPath   string
		wantTeamID string
		wantCard   bool
	}{
		{name: "already usage", usage: &Usage{Unlocked: true, UsagePercent: 0.27}, want: OutcomeAlready},
		{name: "already access", usage: &Usage{}, access: "SAND_ACCESS_STATE_GRANTED", want: OutcomeAlready},
		{name: "team request", teamID: "12345", usage: &Usage{}, want: OutcomeTeamOK, wantPath: pathTeamAccess, wantTeamID: "12345"},
		{name: "personal activated", usage: &Usage{}, want: OutcomeActivated, wantPath: pathStartTrial},
		{name: "card required", usage: &Usage{}, trialBody: `{"error":"cardVerificationRequired","url":"https://checkout.stripe.com/session"}`, want: OutcomeCardRequired, wantPath: pathStartTrial, wantCard: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calledPath string
			var teamPathSeen bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calledPath = r.URL.Path
				if r.URL.Path == pathDashboardMe {
					body := map[string]any{"email": "real@example.com"}
					if tc.teamID != "" {
						body["teamId"] = 12345
					}
					_ = json.NewEncoder(w).Encode(body)
					return
				}
				if r.URL.Path == pathAccessStatus {
					_, _ = io.WriteString(w, `{"state":"`+tc.access+`"}`)
					return
				}
				if r.URL.Path == pathTeamAccess {
					teamPathSeen = true
					var payload map[string]int64
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["teamId"] != 12345 {
						t.Fatalf("team body = %#v, err=%v", payload, err)
					}
				}
				if r.URL.Path == pathStartTrial && tc.trialBody != "" {
					_, _ = io.WriteString(w, tc.trialBody)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			client := New(testAccount(), WithBaseURL(server.URL), WithUsageFetcher(func(context.Context) (*Usage, error) {
				return tc.usage, nil
			}))
			result, err := client.Claim(context.Background())
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if result.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (result=%+v)", result.Outcome, tc.want, result)
			}
			if tc.wantPath == pathTeamAccess && !teamPathSeen {
				t.Fatalf("team endpoint was not called; last path = %q", calledPath)
			}
			if tc.wantPath != "" && tc.wantPath != pathTeamAccess && calledPath != tc.wantPath {
				t.Fatalf("last path = %q, want %q", calledPath, tc.wantPath)
			}
			if tc.wantTeamID != "" && result.TeamID != tc.wantTeamID {
				t.Fatalf("team id = %q, want %q", result.TeamID, tc.wantTeamID)
			}
			if tc.wantCard && (result.CardURL == "" || !strings.Contains(result.CardURL, "checkout.stripe.com")) {
				t.Fatalf("card url = %q", result.CardURL)
			}
		})
	}
}

func TestStatusKeepsPartialResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathDashboardMe:
			_, _ = io.WriteString(w, `{"email":"real@example.com","teamId":7}`)
		case pathAccessStatus:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "permission denied")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	status, err := New(testAccount(), WithBaseURL(server.URL), WithUsageFetcher(func(context.Context) (*Usage, error) {
		return &Usage{Unlocked: true}, nil
	})).Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Usage == nil || status.Me == nil || status.Me.TeamID != "7" {
		t.Fatalf("partial status = %+v", status)
	}
	if status.Access != nil || status.Errors["access"] == "" {
		t.Fatalf("access error missing: %+v", status)
	}
}
