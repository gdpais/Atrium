package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"atrium/tools/patientzero/internal/logschema"
	"atrium/tools/patientzero/internal/oslog"
	"atrium/tools/patientzero/internal/shop"
)

const loadScript = "../../load/patientzero.js"

// The k6 script names the entry points and headers itself; it must name the
// ones the server routes.
func TestLoadScriptUsesTheServersPathsAndHeaders(t *testing.T) {
	src, err := os.ReadFile(loadScript)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{PathOrderList, PathPlaceOrder, PathAPIOrders, HeaderUser, HeaderSession, "X-Forwarded-For"} {
		if !strings.Contains(string(src), "'"+s+"'") {
			t.Errorf("%s does not name %q", loadScript, s)
		}
	}
}

// safeStore is a Store for many concurrent callers; every product in
// traffic.js exists.
type safeStore struct {
	mu        sync.Mutex
	customers map[string]int
}

func (s *safeStore) Customer(_ context.Context, username string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.customers[username]; !ok {
		s.customers[username] = 1000 + len(s.customers)
	}
	return s.customers[username], nil
}

func (s *safeStore) Orders(context.Context, int, int) ([]shop.Order, error) {
	return []shop.Order{{ID: 7, Product: "Thermometer", Quantity: 1, Total: 12.5}}, nil
}

func (s *safeStore) PlaceOrder(_ context.Context, _ int, product, quantity int) (shop.Order, error) {
	if product < 1 || product > 5 {
		return shop.Order{}, fmt.Errorf("%w: product %d does not exist", shop.ErrInvalid, product)
	}
	return shop.Order{ID: 8, Product: "Thermometer", Quantity: quantity, Total: 12.5 * float64(quantity)}, nil
}

type safeRecorder struct {
	mu  sync.Mutex
	rec *recorder
}

func (s *safeRecorder) Write(ctx context.Context, rows []oslog.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(ctx, rows)
}

// The smoke profile against the real handlers: every entry point is called,
// a shopper keeps one username and client IP, and a session belongs to one
// user. Needs k6 on the PATH.
func TestLoadSmokeProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("runs k6 for several seconds")
	}
	k6, err := exec.LookPath("k6")
	if err != nil {
		t.Skip("k6 is not on the PATH")
	}
	types, err := logschema.Load(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	schema := oslog.NewSchema(types)
	logs := &safeRecorder{rec: &recorder{schema: schema}}
	s := &Server{Store: &safeStore{customers: map[string]int{}}, Logs: logs, Schema: schema, FrontEnd: "FE-PZ-1",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	cmd := exec.Command(k6, "run", "--quiet", "--no-color", "--no-usage-report",
		"-e", "BASE_URL="+srv.URL, "-e", "PROFILE=smoke", "-e", "DURATION=6s",
		"-e", "THINK_SCALE=0.02", "-e", "RUN_ID=test", loadScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("k6: %v\n%s", err, out)
	}

	logs.mu.Lock()
	defer logs.mu.Unlock()
	byType := logs.rec.byType()
	for _, lt := range []string{"screen", "mobile_request", "integration", "general"} {
		if len(byType[lt]) == 0 {
			t.Errorf("no %s rows", lt)
		}
	}
	shopper := regexp.MustCompile(`^shopper\.(\d{4})$`)
	session := regexp.MustCompile(`^test\.v(\d{4})\.\d+$`)
	ipOf := map[string]string{}
	userOf := map[string]string{}
	for _, cols := range byType["screen"] {
		user, _ := cols["Username"].(string)
		sess, _ := cols["Session_Id"].(string)
		ip, _ := cols["Client_IP"].(string)
		sm := session.FindStringSubmatch(sess)
		if sm == nil {
			t.Fatalf("screen row session %q is not test.v<n>.<session>", sess)
		}
		// An anonymous session has no username but the visitor's address.
		if want := clientIPFor(20, sm[1]); ip != want {
			t.Errorf("session %s comes from %s, want %s", sess, ip, want)
		}
		if user == "" {
			continue
		}
		um := shopper.FindStringSubmatch(user)
		if um == nil || um[1] != sm[1] {
			t.Errorf("session %s belongs to %q, want shopper.%s", sess, user, sm[1])
		}
		if prev, ok := ipOf[user]; ok && prev != ip {
			t.Errorf("%s came from %s and %s", user, prev, ip)
		}
		ipOf[user] = ip
		if prev, ok := userOf[sess]; ok && prev != user {
			t.Errorf("session %s used by %s and %s", sess, prev, user)
		}
		userOf[sess] = user
	}
	if len(ipOf) != 2 {
		t.Errorf("signed-in shoppers %v, want the smoke profile's 2", slices.Sorted(maps.Keys(ipOf)))
	}
	for _, cols := range byType["integration"] {
		if ip := cols["Source"]; ip != "10.30.0.1" {
			t.Errorf("API call from %v, want the smoke profile's one partner at 10.30.0.1", ip)
		}
	}
}

// clientIPFor mirrors the script's clientIP for a four-digit user number.
func clientIPFor(b int, n string) string {
	var i int
	fmt.Sscanf(n, "%d", &i)
	return fmt.Sprintf("10.%d.%d.%d", b, (i-1)/254, (i-1)%254+1)
}
