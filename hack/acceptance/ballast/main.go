// Command ballast is the memory-ballast workload for the suspend/resume
// acceptance test. It makes no LLM calls. It allocates a fixed block of
// memory, writes every byte of it, and keeps the block reachable for the
// lifetime of the process. The acceptance test provisions an actor from this
// image, snapshots and restores it, and checks the result from the reports the
// process posts to the development host.
//
// The block is filled page by page. The content of page i is a deterministic
// pseudo-random stream seeded by i, so no two pages are equal, the block does
// not compress or deduplicate, and the bytes of a page depend on that page's
// position. A page that a restore swaps with another, shifts, or fills only
// partially therefore differs from the bytes its own page number implies.
//
// The process has two sides:
//
//   - It answers the wakeup probe on port 8081 at /readyz, so Substrate can
//     resume it. The body reports the ballast size, the resident set size and
//     the checksum computed at startup.
//   - It POSTs a report every two seconds to CASTWORKS_CONTROL_PLANE over
//     cleartext HTTP/2 with prior knowledge. That name is the development
//     host's selectorless Service, castworks-dev.castworks-system.svc.cluster.local
//     on port 9090, and the actor's EgressPolicy allows it with an http rule.
//     This is the same egress path the warden uses, so the test never needs a
//     port-forward into ate-system.
//
// Each report carries the startup ID and startup time (both live only in
// memory), the process ID, the ballast size and checksum, the resident set
// size, and the result of a fresh page-by-page verification: whether every
// page still matches the content its page number implies, the first page that
// differs, and the byte offset of its first differing byte. The report
// sequence advances only after the host accepts a report, so a report after a
// restore with the same startup ID and a higher sequence proves that the
// snapshot restored the same process and its memory.
//
// The image is built for linux/arm64 only. Dockerfile and build.sh in this
// directory record the tag and the build commands.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// ballastMiB is the amount the actor holds resident. The go criterion
	// measures snapshot restore with an actor of about 500 MB, so the value is
	// fixed rather than a flag.
	ballastMiB   = 500
	ballastBytes = ballastMiB << 20
	// pageSize is the unit of fill and verification. It is a constant rather
	// than os.Getpagesize() so that the content of a page depends only on its
	// page number and is reproducible on any host.
	pageSize = 4096

	readyAddr = ":8081"
	readyPath = "/readyz"

	reportPath = "/report"

	// The actor template mounts the system-info volume here; the file holds the
	// actor name Substrate assigned.
	defaultActorDir = "/run/ate"
	actorNameFile   = "actor-name"

	defaultReportInterval = 2 * time.Second
	defaultBackoffMin     = 1 * time.Second
	defaultBackoffMax     = 10 * time.Second
	defaultReportTimeout  = 10 * time.Second
)

// ballast is deliberately a package-level variable so the block stays
// reachable and the garbage collector never reclaims it.
var ballast []byte

type readyResponse struct {
	Status       string `json:"status"`
	BallastBytes int    `json:"ballast_bytes"`
	BallastMiB   int    `json:"ballast_mib"`
	CRC32        string `json:"crc32"`
	RSSBytes     int64  `json:"rss_bytes"`
	RSSError     string `json:"rss_error,omitempty"`
	StartedAt    string `json:"started_at"`
}

// report is the JSON body of one report POST. The acceptance test mirrors it.
type report struct {
	ActorName    string         `json:"actor_name"`
	StartupID    string         `json:"startup_id"`
	StartedAt    string         `json:"started_at"`
	Sequence     int64          `json:"sequence"`
	PID          int            `json:"pid"`
	BallastBytes int            `json:"ballast_bytes"`
	BallastMiB   int            `json:"ballast_mib"`
	CRC32        string         `json:"crc32"`
	RSSBytes     int64          `json:"rss_bytes"`
	RSSError     string         `json:"rss_error,omitempty"`
	ReportAt     string         `json:"report_at"`
	Verify       verifyResponse `json:"verify"`
}

type verifyResponse struct {
	Match    bool `json:"match"`
	Pages    int  `json:"pages"`
	PageSize int  `json:"page_size"`
	// FirstMismatchPage is the zero-based number of the first page whose bytes
	// differ from the content its page number implies. It is omitted when
	// match is true.
	FirstMismatchPage *int `json:"first_mismatch_page,omitempty"`
	// FirstMismatchOffset is the byte offset of the first differing byte. It
	// is omitted when match is true.
	FirstMismatchOffset *int64 `json:"first_mismatch_offset_bytes,omitempty"`
	ScanMS              int64  `json:"scan_ms"`
}

func main() {
	addr := flag.String("addr", readyAddr, "wakeup probe listen address")
	flag.Parse()

	actorName, err := readActorName(defaultActorDir)
	if err != nil {
		log.Fatalf("ballast: read actor name: %v", err)
	}
	target := os.Getenv("CASTWORKS_CONTROL_PLANE")
	if target == "" {
		log.Fatal("ballast: CASTWORKS_CONTROL_PLANE is not set")
	}

	startedAt := time.Now().UTC()
	startedAtText := startedAt.Format(time.RFC3339Nano)

	ballast = fill(ballastBytes)
	startCRC := crc32.ChecksumIEEE(ballast)
	startCRCText := fmt.Sprintf("%08x", startCRC)

	startupID, err := randomHex(16)
	if err != nil {
		log.Fatalf("ballast: generate startup ID: %v", err)
	}

	rss, rssErr := residentBytes()
	if rssErr != nil {
		log.Printf("ballast: resident set size unavailable: %v", rssErr)
	}
	log.Printf("ballast filled: actor=%s startup=%s bytes=%d pages=%d crc32=%s rss_bytes=%d at=%s",
		actorName, startupID, len(ballast), numPages(len(ballast)), startCRCText, rss, startedAtText)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go serveReady(ctx, *addr, startedAtText, startCRCText)

	runReporter(ctx, reporterConfig{
		URL:       "http://" + target + reportPath,
		ActorName: actorName,
		StartupID: startupID,
		StartedAt: startedAtText,
		CRC32:     startCRCText,
		PID:       os.Getpid(),
		Client:    newH2CClient(),
	})
}

// reporterConfig carries everything runReporter needs. The zero timeouts fall
// back to the defaults so tests only override what they care about.
type reporterConfig struct {
	URL        string
	ActorName  string
	StartupID  string
	StartedAt  string
	CRC32      string
	PID        int
	Interval   time.Duration
	BackoffMin time.Duration
	BackoffMax time.Duration
	Client     *http.Client
}

func (c reporterConfig) withDefaults() reporterConfig {
	if c.Interval <= 0 {
		c.Interval = defaultReportInterval
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = defaultBackoffMin
	}
	if c.BackoffMax < c.BackoffMin {
		c.BackoffMax = defaultBackoffMax
	}
	if c.Client == nil {
		c.Client = newH2CClient()
	}
	return c
}

// runReporter sends one report at a time and returns when ctx is cancelled.
// The sequence advances only after the host accepts a report, so the first
// accepted report carries 1 and a restored process continues from the value it
// had before the snapshot. A failed report is retried with a backoff, because
// a new actor's EgressPolicy takes a few seconds to propagate.
func runReporter(ctx context.Context, cfg reporterConfig) {
	cfg = cfg.withDefaults()
	backoff := cfg.BackoffMin
	var sequence int64
	for {
		next := sequence + 1
		rep := buildReport(cfg, next)
		err := sendReport(ctx, cfg.Client, cfg.URL, rep)

		wait := cfg.Interval
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("ballast: report %d failed: %v; retrying in %s", next, err, backoff)
			wait = backoff
			backoff *= 2
			if backoff > cfg.BackoffMax {
				backoff = cfg.BackoffMax
			}
		} else {
			sequence = next
			backoff = cfg.BackoffMin
			if !rep.Verify.Match {
				log.Printf("ballast: report %d: page %s differs at offset %s",
					next, optionalInt(rep.Verify.FirstMismatchPage), optionalInt64(rep.Verify.FirstMismatchOffset))
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// buildReport scans the ballast page by page and collects the fields of one
// report. The scan runs for every report, so a report received after a restore
// always describes the memory as it is at that moment.
func buildReport(cfg reporterConfig, sequence int64) report {
	begin := time.Now()
	match, firstPage, firstOffset := verify(ballast)
	scanMS := time.Since(begin).Milliseconds()

	rss, rssErr := residentBytes()
	rep := report{
		ActorName:    cfg.ActorName,
		StartupID:    cfg.StartupID,
		StartedAt:    cfg.StartedAt,
		Sequence:     sequence,
		PID:          cfg.PID,
		BallastBytes: len(ballast),
		BallastMiB:   ballastMiB,
		CRC32:        cfg.CRC32,
		RSSBytes:     rss,
		ReportAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Verify: verifyResponse{
			Match:    match,
			Pages:    numPages(len(ballast)),
			PageSize: pageSize,
			ScanMS:   scanMS,
		},
	}
	if rssErr != nil {
		rep.RSSError = rssErr.Error()
	}
	if !match {
		rep.Verify.FirstMismatchPage = &firstPage
		rep.Verify.FirstMismatchOffset = &firstOffset
	}
	return rep
}

// newH2CClient returns an HTTP client that speaks cleartext HTTP/2 with prior
// knowledge, as the EgressPolicy's http rule allows.
func newH2CClient() *http.Client {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{
		Transport: &http.Transport{Protocols: &protocols},
		Timeout:   defaultReportTimeout,
	}
}

func sendReport(ctx context.Context, client *http.Client, url string, rep report) error {
	body, err := json.Marshal(rep)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("host answered %s", resp.Status)
	}
	return nil
}

// serveReady serves the wakeup probe until ctx is cancelled. Without this
// listener ResumeActor can never succeed, so a bind failure ends the process.
func serveReady(ctx context.Context, addr, startedAt, crc string) {
	mux := http.NewServeMux()
	mux.HandleFunc(readyPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rss, err := residentBytes()
		resp := readyResponse{
			Status:       "ok",
			BallastBytes: len(ballast),
			BallastMiB:   ballastMiB,
			CRC32:        crc,
			RSSBytes:     rss,
			StartedAt:    startedAt,
		}
		if err != nil {
			resp.RSSError = err.Error()
		}
		writeJSON(w, http.StatusOK, resp)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("ballast: shutdown: %v", err)
		}
	}()

	log.Printf("ballast: readyz listening addr=%s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("ballast: readyz listener: %v", err)
	}
	log.Printf("ballast: shutdown complete")
}

// readActorName reads and trims the actor name Substrate wrote into the
// system-info mount.
func readActorName(dir string) (string, error) {
	path := filepath.Join(dir, actorNameFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return name, nil
}

// randomHex returns n random bytes encoded as lowercase hexadecimal.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// fill allocates size bytes and writes every one of them. Each page is filled
// from its own deterministic stream, so every page faults in and the block
// cannot be compressed or deduplicated into zero or repeated pages.
func fill(size int) []byte {
	b := make([]byte, size)
	for page := 0; page < numPages(size); page++ {
		start := page * pageSize
		end := start + pageSize
		if end > len(b) {
			end = len(b)
		}
		fillPage(b[start:end], page)
	}
	return b
}

// fillPage writes the deterministic content of one page. The page number seeds
// a SplitMix64 stream. SplitMix64 passes the usual statistical tests and has
// good avalanche, so page i is incompressible and never equals page j.
func fillPage(dst []byte, page int) {
	state := uint64(page)
	i := 0
	for ; i+8 <= len(dst); i += 8 {
		binary.LittleEndian.PutUint64(dst[i:], splitmix64(&state))
	}
	if i < len(dst) {
		var tail [8]byte
		binary.LittleEndian.PutUint64(tail[:], splitmix64(&state))
		copy(dst[i:], tail[:len(dst)-i])
	}
}

// splitmix64 advances the SplitMix64 state and returns the next word.
func splitmix64(state *uint64) uint64 {
	*state += 0x9e3779b97f4a7c15
	z := *state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// verify regenerates every page from its page number and compares it with the
// page in memory. It returns the number of the first page that differs and the
// byte offset of its first differing byte. Reporting the page number, not just
// a checksum over the whole block, is what lets the acceptance test tell which
// page a restore corrupted, swapped or shifted.
func verify(b []byte) (match bool, firstPage int, firstOffset int64) {
	expected := make([]byte, pageSize)
	for page := 0; page < numPages(len(b)); page++ {
		start := page * pageSize
		end := start + pageSize
		if end > len(b) {
			end = len(b)
		}
		want := expected[:end-start]
		fillPage(want, page)
		if bytes.Equal(b[start:end], want) {
			continue
		}
		offset := int64(start)
		for i := range want {
			if b[start+i] != want[i] {
				offset = int64(start + i)
				break
			}
		}
		return false, page, offset
	}
	return true, -1, -1
}

// numPages is the number of pageSize pages that cover size bytes.
func numPages(size int) int {
	return (size + pageSize - 1) / pageSize
}

// residentBytes reads the resident set size from /proc/self/statm. The second
// field is the number of resident pages.
func residentBytes() (int64, error) {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected /proc/self/statm contents %q", data)
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse resident pages: %w", err)
	}
	return pages * int64(os.Getpagesize()), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("ballast: write response: %v", err)
	}
}

// optionalInt renders an optional page number for a log line.
func optionalInt(value *int) string {
	if value == nil {
		return "unknown"
	}
	return strconv.Itoa(*value)
}

// optionalInt64 renders an optional byte offset for a log line.
func optionalInt64(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatInt(*value, 10)
}
