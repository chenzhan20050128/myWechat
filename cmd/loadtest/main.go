// Command loadtest drives a realistic HTTP workload against a running API.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
)

const password = "Load2026x"

type options struct {
	base            string
	report          string
	users           int
	friendsPerUser  int
	messagesPerConv int
	momentsPerUser  int
	concurrency     int
	targetRPS       int
	duration        time.Duration
	timeout         time.Duration
	scenario        string
}

type envelope struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	RequestID string          `json:"request_id"`
}

type tokenPair struct {
	UserID           string    `json:"user_id"`
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	DeviceID         string    `json:"device_id"`
}

type loadUser struct {
	ID          int64    `json:"id"`
	Phone       string   `json:"phone"`
	Account     string   `json:"account"`
	AccessToken string   `json:"access_token"`
	ConvIDs     []string `json:"conversation_ids"`
}

type loadConversation struct {
	ID   string `json:"id"`
	AIdx int    `json:"a"`
	BIdx int    `json:"b"`
}

type loadState struct {
	Users         []loadUser         `json:"users"`
	Conversations []loadConversation `json:"conversations"`
}

type client struct {
	http    *http.Client
	timeout time.Duration
	ip      atomic.Uint64
}

type requestResult struct {
	Scenario string
	Status   int
	Elapsed  time.Duration
	Bytes    int64
}

type sample struct {
	Scenario string
	Status   int
	Elapsed  time.Duration
	Bytes    int64
}

type summary struct {
	Scenario    string         `json:"scenario"`
	Count       int64          `json:"count"`
	Errors      int64          `json:"errors"`
	RateLimited int64          `json:"rate_limited"`
	ClientErr   int64          `json:"client_errors"`
	ServerErr   int64          `json:"server_errors"`
	Bytes       int64          `json:"response_bytes"`
	QPS         float64        `json:"qps"`
	MinMS       float64        `json:"min_ms"`
	P50MS       float64        `json:"p50_ms"`
	P90MS       float64        `json:"p90_ms"`
	P95MS       float64        `json:"p95_ms"`
	P99MS       float64        `json:"p99_ms"`
	MaxMS       float64        `json:"max_ms"`
	StatusCodes map[string]int `json:"status_codes"`
}

type internalSummary struct {
	summary
	latencies []time.Duration
}

type report struct {
	StartedAt   time.Time `json:"started_at"`
	ElapsedMS   int64     `json:"elapsed_ms"`
	Scenario    string    `json:"scenario"`
	Concurrency int       `json:"concurrency"`
	TargetRPS   int       `json:"target_rps"`
	Users       int       `json:"users"`
	Results     []summary `json:"results"`
}

type pacer struct {
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

func (p *pacer) wait(ctx context.Context) error {
	if p.interval <= 0 {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	next := p.next
	p.next = p.next.Add(p.interval)
	p.mu.Unlock()

	wait := time.Until(next)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func main() {
	o := &options{}
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.StringVar(&o.base, "base", "http://127.0.0.1:8080", "API base URL")
	fs.StringVar(&o.report, "report", ".tmp/loadtest-report.json", "JSON report path")
	fs.IntVar(&o.users, "users", 50, "virtual accounts to create")
	fs.IntVar(&o.friendsPerUser, "friends-per-user", 4, "new friends created per account")
	fs.IntVar(&o.messagesPerConv, "messages-per-conversation", 20, "seed text messages per conversation")
	fs.IntVar(&o.momentsPerUser, "moments-per-user", 10, "seed moments per account")
	fs.IntVar(&o.concurrency, "concurrency", 64, "closed-loop worker goroutines")
	fs.IntVar(&o.targetRPS, "target-rps", 200, "desired request rate; 0 means unlimited")
	fs.DurationVar(&o.duration, "duration", 60*time.Second, "measurement duration")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Second, "per-request timeout")
	fs.StringVar(&o.scenario, "scenario", "mixed", "mixed|login|profile|friends|conversations|history|send|feed")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	o.base = strings.TrimRight(o.base, "/")
	if o.users < 10 {
		fatal("users must be at least 10")
	}
	if o.friendsPerUser < 1 {
		fatal("friends-per-user must be at least 1")
	}
	switch o.scenario {
	case "mixed", "login", "profile", "friends", "conversations", "history", "send", "feed":
	default:
		fatal("unknown scenario " + o.scenario)
	}

	c := &client{
		http: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        max(1024, o.concurrency*4),
				MaxIdleConnsPerHost: max(256, o.concurrency),
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true,
			},
		},
		timeout: o.timeout,
	}
	ctx := context.Background()

	fmt.Printf("checking API at %s\n", o.base)
	status, _, _, err := c.do(ctx, http.MethodGet, o.base+"/healthz", "", false, nil)
	if err != nil {
		fatal("API health check: %v", err)
	}
	if status != http.StatusOK {
		fatal("API health check returned %d", status)
	}

	fmt.Printf("seeding %d users, %d friends/user, %d messages/conversation, %d moments/user\n",
		o.users, o.friendsPerUser, o.messagesPerConv, o.momentsPerUser)
	started := time.Now()
	state, err := seed(ctx, c, o)
	if err != nil {
		fatal("seed: %v", err)
	}
	fmt.Printf("seed completed in %s\n", time.Since(started).Round(time.Millisecond))

	rep, err := run(ctx, c, o, state)
	if err != nil {
		fatal("load test: %v", err)
	}
	printReport(rep)
	if err := writeReport(o.report, rep); err != nil {
		fatal("write report: %v", err)
	}
	fmt.Printf("report written to %s\n", o.report)
}

func seed(ctx context.Context, c *client, o *options) (*loadState, error) {
	state := &loadState{Users: make([]loadUser, o.users)}
	if err := seedUsers(ctx, c, o, state); err != nil {
		return nil, err
	}
	if err := seedFriendships(ctx, c, o, state); err != nil {
		return nil, err
	}
	if err := seedMoments(ctx, c, o, state); err != nil {
		return nil, err
	}
	if err := seedMessages(ctx, c, o, state); err != nil {
		return nil, err
	}
	return state, nil
}

func seedUsers(ctx context.Context, c *client, o *options, state *loadState) error {
	tags := newTag()
	work := make(chan int, o.users)
	for i := range o.users {
		work <- i
	}
	close(work)

	g, gctx := newGroup(ctx)
	for range min(8, o.users) {
		g.Go(func() error {
			for i := range work {
				select {
				case <-gctx.Done():
					return gctx.Err()
				default:
				}
				tag := tags.next(i)
				digits, err := randomPhoneDigits()
				if err != nil {
					return err
				}
				phone := "+8613" + digits
				account := "bt" + tag + "u" + fmt.Sprintf("%04d", i)
				body := map[string]any{
					"phone": phone, "password": password, "account_name": account,
					"nickname": fmt.Sprintf("Load User %04d", i),
					"device":   randomDevice("ios"),
				}
				status, data, code, err := c.requestRetry(gctx, http.MethodPost, o.base+"/api/v1/auth/register", "", body)
				if err != nil {
					return err
				}
				if status != http.StatusCreated || code != "OK" {
					return fmt.Errorf("register %s: status=%d code=%s body=%s", account, status, code, truncate(data))
				}
				var pair tokenPair
				if err := json.Unmarshal(data, &pair); err != nil {
					return fmt.Errorf("decode register response: %w", err)
				}
				id, err := parseInt64(pair.UserID)
				if err != nil {
					return err
				}
				state.Users[i] = loadUser{
					ID: id, Phone: phone, Account: account, AccessToken: pair.AccessToken,
					ConvIDs: make([]string, 0),
				}
			}
			return nil
		})
	}
	return g.Wait()
}

func seedFriendships(ctx context.Context, c *client, o *options, state *loadState) error {
	seen := make(map[[2]int64]struct{})
	for i := range state.Users {
		for d := 1; d <= o.friendsPerUser; d++ {
			j := (i + d) % o.users
			if i == j {
				continue
			}
			lo, hi := state.Users[i].ID, state.Users[j].ID
			key := [2]int64{min64(lo, hi), max64(lo, hi)}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}

			createBody := map[string]any{
				"source": "account_name", "account_name": state.Users[j].Account,
				"verify_text": "loadtest contact",
			}
			status, data, code, err := c.requestRetry(ctx, http.MethodPost,
				o.base+"/api/v1/contacts/requests", state.Users[i].AccessToken, createBody)
			if err != nil {
				return err
			}
			if status != http.StatusCreated || code != "OK" {
				return fmt.Errorf("friend request %d->%d: status=%d code=%s body=%s",
					state.Users[i].ID, state.Users[j].ID, status, code, truncate(data))
			}
			var requestView struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(data, &requestView); err != nil {
				return err
			}

			status, data, code, err = c.requestRetry(ctx, http.MethodPost,
				o.base+"/api/v1/contacts/requests/"+requestView.ID+"/accept", state.Users[j].AccessToken, map[string]any{})
			if err != nil {
				return err
			}
			if status != http.StatusOK || code != "OK" {
				return fmt.Errorf("accept friend request %s: status=%d code=%s body=%s",
					requestView.ID, status, code, truncate(data))
			}
			var acceptView struct {
				ConversationID string `json:"conversation_id"`
			}
			if err := json.Unmarshal(data, &acceptView); err != nil {
				return err
			}
			state.Conversations = append(state.Conversations, loadConversation{ID: acceptView.ConversationID, AIdx: i, BIdx: j})
			state.Users[i].ConvIDs = append(state.Users[i].ConvIDs, acceptView.ConversationID)
			state.Users[j].ConvIDs = append(state.Users[j].ConvIDs, acceptView.ConversationID)
		}
	}
	if len(state.Conversations) == 0 {
		return fmt.Errorf("no conversations were created")
	}
	return nil
}

func seedMoments(ctx context.Context, c *client, o *options, state *loadState) error {
	work := make(chan int, o.users)
	for i := range o.users {
		work <- i
	}
	close(work)

	g, gctx := newGroup(ctx)
	for range min(8, o.users) {
		g.Go(func() error {
			for i := range work {
				select {
				case <-gctx.Done():
					return gctx.Err()
				default:
				}
				for n := 0; n < o.momentsPerUser; n++ {
					body := map[string]any{
						"content":    fmt.Sprintf("Morning run %d km, then coffee and reading for %d minutes", (n+i)%8+1, (n+i)%40+10),
						"visibility": "all_friends",
						"country":    "CN",
						"province":   "Shanghai",
						"city":       "Shanghai",
						"place_name": fmt.Sprintf("Jing'an District Park %d", (n+i)%12+1),
					}
					status, data, code, err := c.requestRetry(gctx, http.MethodPost,
						o.base+"/api/v1/moments", state.Users[i].AccessToken, body)
					if err != nil {
						return err
					}
					if status != http.StatusOK || code != "OK" {
						return fmt.Errorf("publish moment user=%d n=%d: status=%d code=%s body=%s",
							state.Users[i].ID, n, status, code, truncate(data))
					}
				}
			}
			return nil
		})
	}
	return g.Wait()
}

func seedMessages(ctx context.Context, c *client, o *options, state *loadState) error {
	tags := newTag()
	work := make(chan loadConversation, len(state.Conversations))
	for _, conv := range state.Conversations {
		work <- conv
	}
	close(work)

	g, gctx := newGroup(ctx)
	for range min(16, len(state.Conversations)) {
		g.Go(func() error {
			for conv := range work {
				select {
				case <-gctx.Done():
					return gctx.Err()
				default:
				}
				for n := 0; n < o.messagesPerConv; n++ {
					sender := state.Users[conv.AIdx]
					if n%2 == 1 {
						sender = state.Users[conv.BIdx]
					}
					payload := map[string]string{
						"content": fmt.Sprintf("message %02d: dinner around 7, bring the charging cable and the travel adapter", n+1),
					}
					body := map[string]any{
						"client_msg_id": tags.next(conv.AIdx*100000 + conv.BIdx*100 + n),
						"type":          "text",
						"payload":       payload,
					}
					status, data, code, err := c.requestRetry(gctx, http.MethodPost,
						o.base+"/api/v1/conversations/"+conv.ID+"/messages", sender.AccessToken, body)
					if err != nil {
						return err
					}
					if status != http.StatusOK || code != "OK" {
						return fmt.Errorf("seed message conv=%s n=%d: status=%d code=%s body=%s",
							conv.ID, n, status, code, truncate(data))
					}
				}
			}
			return nil
		})
	}
	return g.Wait()
}

func run(ctx context.Context, c *client, o *options, state *loadState) (*report, error) {
	p := &pacer{interval: 0}
	if o.targetRPS > 0 {
		p.interval = time.Second / time.Duration(o.targetRPS)
	}
	ctx, cancel := context.WithTimeout(ctx, o.duration)
	defer cancel()

	counter := &atomic.Uint64{}
	results := make(chan sample, max(1024, o.concurrency*4))
	started := time.Now()
	collector := newCollector(results, started)
	collector.start()

	wg := &sync.WaitGroup{}
	for worker := 0; worker < o.concurrency; worker++ {
		local := &atomic.Uint64{}
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if err := p.wait(ctx); err != nil {
					return
				}
				seq := counter.Add(1)
				// Each worker cycles accounts independently. Using the global
				// sequence here would correlate account ids with weighted
				// scenario slots and create accidental hot users.
				idx := int((local.Add(1) - 1 + uint64(worker)) % uint64(o.users))
				user := &state.Users[idx]
				scenario := chooseScenario(o.scenario, seq)
				res := execute(ctx, c, o, state, user, scenario, seq)
				select {
				case results <- sample{Scenario: scenario, Status: res.status, Elapsed: res.elapsed, Bytes: res.bytes}:
				case <-ctx.Done():
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	cancel()
	return collector.finish(o, started), nil
}

type response struct {
	status  int
	elapsed time.Duration
	bytes   int64
}

func execute(ctx context.Context, c *client, o *options, state *loadState, user *loadUser, scenario string, seq uint64) response {
	start := time.Now()
	var status int
	var bytes int64
	var err error
	switch scenario {
	case "login":
		body := map[string]any{
			"login_id": user.Account, "password": password, "device": randomDevice("android"),
		}
		status, _, _, err = c.do(ctx, http.MethodPost, o.base+"/api/v1/auth/login", "", true, body)
	case "profile":
		status, _, _, err = c.do(ctx, http.MethodGet, o.base+"/api/v1/users/me", user.AccessToken, false, nil)
	case "friends":
		status, _, _, err = c.do(ctx, http.MethodGet, o.base+"/api/v1/contacts/friends?limit=20", user.AccessToken, false, nil)
	case "conversations":
		status, _, _, err = c.do(ctx, http.MethodGet, o.base+"/api/v1/conversations", user.AccessToken, false, nil)
	case "history":
		conv := user.ConvIDs[int(seq)%len(user.ConvIDs)]
		status, _, _, err = c.do(ctx, http.MethodGet,
			o.base+"/api/v1/conversations/"+conv+"/messages?limit=20", user.AccessToken, false, nil)
	case "send":
		conv := user.ConvIDs[int(seq)%len(user.ConvIDs)]
		body := map[string]any{
			"client_msg_id": uuid.NewString(),
			"type":          "text",
			"payload":       map[string]string{"content": fmt.Sprintf("live check %d: call me after standup", seq)},
		}
		status, _, _, err = c.do(ctx, http.MethodPost,
			o.base+"/api/v1/conversations/"+conv+"/messages", user.AccessToken, false, body)
	case "feed":
		status, _, _, err = c.do(ctx, http.MethodGet,
			o.base+"/api/v1/moments/feed?limit=10", user.AccessToken, false, nil)
	default:
		err = fmt.Errorf("unknown scenario %q", scenario)
	}
	elapsed := time.Since(start)
	if err != nil {
		// Timeouts and connection resets are latency samples with status 0.
		status = 0
	}
	return response{status: status, elapsed: elapsed, bytes: bytes}
}

func chooseScenario(defaultScenario string, seq uint64) string {
	if defaultScenario != "mixed" {
		return defaultScenario
	}
	n := int(seq % 100)
	switch {
	case n < 8:
		return "login"
	case n < 33:
		return "profile"
	case n < 43:
		return "friends"
	case n < 61:
		return "conversations"
	case n < 81:
		return "history"
	case n < 88:
		return "send"
	default:
		return "feed"
	}
}

func (c *client) do(ctx context.Context, method, url, token string, spoofIP bool, body any) (int, json.RawMessage, string, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, "", err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mywechat-loadtest/1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if spoofIP {
		req.Header.Set("X-Forwarded-For", c.nextIP())
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, "", err
	}
	var env envelope
	code := ""
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err == nil {
			code = env.Code
			if resp.StatusCode >= 400 {
				// Keep the full envelope for actionable setup errors; data is
				// not consumed when a request fails.
				return resp.StatusCode, json.RawMessage(raw), code, nil
			}
			return resp.StatusCode, env.Data, code, nil
		}
	}
	return resp.StatusCode, nil, code, nil
}

func (c *client) requestRetry(ctx context.Context, method, url, token string, body any) (int, json.RawMessage, string, error) {
	var lastStatus int
	var lastData json.RawMessage
	var lastCode string
	for attempt := 0; attempt < 6; attempt++ {
		status, data, code, err := c.do(ctx, method, url, token, true, body)
		if err != nil {
			return status, data, code, err
		}
		if status != http.StatusTooManyRequests {
			return status, data, code, nil
		}
		lastStatus, lastData, lastCode = status, data, code
		select {
		case <-ctx.Done():
			return lastStatus, lastData, lastCode, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	return lastStatus, lastData, lastCode, fmt.Errorf("still rate limited after retries")
}

func (c *client) nextIP() string {
	n := c.ip.Add(1)
	return fmt.Sprintf("10.%d.%d.%d", (n>>16)&255, (n>>8)&255, n&255)
}

func newCollector(input chan sample, started time.Time) *collector {
	return &collector{input: input, started: make(chan struct{}), runStarted: started}
}

type collector struct {
	input      chan sample
	started    chan struct{}
	runStarted time.Time
	mu         sync.Mutex
	byName     map[string]*internalSummary
}

func (c *collector) start() {
	go func() {
		for s := range c.input {
			c.add(s)
		}
	}()
	close(c.started)
}

func (c *collector) wait() error {
	<-c.started
	return nil
}

func (c *collector) add(s sample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byName == nil {
		c.byName = make(map[string]*internalSummary)
	}
	su, ok := c.byName[s.Scenario]
	if !ok {
		su = &internalSummary{summary: summary{Scenario: s.Scenario, StatusCodes: make(map[string]int)}}
		c.byName[s.Scenario] = su
	}
	su.Count++
	su.Bytes += s.Bytes
	su.StatusCodes[fmt.Sprint(s.Status)]++
	su.latencies = append(su.latencies, s.Elapsed)
	switch {
	case s.Status == 429:
		su.RateLimited++
	case s.Status == 0 || s.Status >= 500:
		su.ServerErr++
	case s.Status >= 400:
		su.ClientErr++
	}
}

func (c *collector) finish(o *options, started time.Time) *report {
	rep := &report{
		Scenario: o.scenario, Concurrency: o.concurrency, TargetRPS: o.targetRPS,
		Users: o.users, StartedAt: started,
		ElapsedMS: time.Since(started).Milliseconds(),
	}
	// finish is called after workers exit; closing here is safe because no
	// producer is left running.
	close(c.input)
	c.mu.Lock()
	elapsed := time.Since(started).Seconds()
	for _, is := range c.byName {
		su := is.summary
		su.QPS = float64(su.Count) / elapsed
		sort.Slice(is.latencies, func(i, j int) bool { return is.latencies[i] < is.latencies[j] })
		if len(is.latencies) > 0 {
			su.MinMS = ms(is.latencies[0])
			su.MaxMS = ms(is.latencies[len(is.latencies)-1])
			su.P50MS = ms(percentile(is.latencies, .50))
			su.P90MS = ms(percentile(is.latencies, .90))
			su.P95MS = ms(percentile(is.latencies, .95))
			su.P99MS = ms(percentile(is.latencies, .99))
		}
		su.Errors = su.ServerErr + su.ClientErr
		rep.Results = append(rep.Results, su)
	}
	c.mu.Unlock()
	sort.Slice(rep.Results, func(i, j int) bool { return rep.Results[i].Scenario < rep.Results[j].Scenario })
	return rep
}

func printReport(rep *report) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SCENARIO\tCOUNT\tQPS\tP50_MS\tP95_MS\tP99_MS\tNON_LIMIT_ERR\t4XX\t429\t5XX")
	for _, s := range rep.Results {
		fmt.Fprintf(w, "%s\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t%d\t%d\t%d\t%d\n",
			s.Scenario, s.Count, s.QPS, s.P50MS, s.P95MS, s.P99MS, s.Errors, s.ClientErr, s.RateLimited, s.ServerErr)
	}
	_ = w.Flush()
}

func writeReport(path string, rep *report) error {
	if err := os.MkdirAll(".tmp", 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func randomDevice(platform string) map[string]string {
	return map[string]string{
		"device_id":   uuid.NewString(),
		"device_name": "iPhone 15 Pro",
		"platform":    platform,
	}
}

func randomPhoneDigits() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%09d", n.Int64()), nil
}

func newGroup(ctx context.Context) (*workerGroup, context.Context) {
	cctx, cancel := context.WithCancel(ctx)
	return &workerGroup{cancel: cancel}, cctx
}

type workerGroup struct {
	wg     sync.WaitGroup
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (g *workerGroup) Go(fn func() error) {
	g.wg.Add(1)
	go func() (err error) {
		defer g.wg.Done()
		err = fn()
		if err != nil {
			g.cancel()
		}
		g.once.Do(func() { g.err = err })
		return
	}()
}

func (g *workerGroup) Wait() error {
	g.wg.Wait()
	g.cancel()
	return g.err
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	idx := int(float64(len(values)-1) * p)
	return values[idx]
}

func ms(v time.Duration) float64 {
	return float64(v) / float64(time.Millisecond)
}

type tagger struct {
	mu  sync.Mutex
	tag string
}

func newTag() *tagger {
	return &tagger{tag: fmt.Sprintf("%09x", time.Now().UnixNano()/int64(time.Millisecond)%0x1000000000)}
}

func (t *tagger) next(i int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return fmt.Sprintf("%s%04d", t.tag, i)
}

func parseInt64(s string) (int64, error) {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

func truncate(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 240 {
		return s[:240] + "..."
	}
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "loadtest: "+format+"\n", args...)
	os.Exit(1)
}
