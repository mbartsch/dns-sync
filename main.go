package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
)

var (
	version = "dev"
	commit  = ""
)

func versionString() string {
	if commit == "" {
		return version
	}
	return version + "-" + commit
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	DockerSocket      string // DOCKER_SOCKET         default: /var/run/docker.sock
	RedisAddr         string // REDIS_ADDR             default: 127.0.0.1:6379
	RedisPass         string // REDIS_PASSWORD[_FILE]
	RedisPrefix       string // REDIS_PREFIX           default: _dns:
	Zone              string // DNS_ZONE               default: srv.bartsch.red.
	Hostname          string // HOSTNAME               default: os.Hostname()
	TTL               int    // DNS_TTL                default: 30
	Network           string // DOCKER_NETWORK         if set, prefer IP from this network
	StaticRecordsFile string // STATIC_RECORDS_FILE    optional YAML/JSON of static records
}

func loadConfig() Config {
	hostname, _ := os.Hostname()
	return Config{
		DockerSocket:      envOr("DOCKER_SOCKET", "/var/run/docker.sock"),
		RedisAddr:         envOr("REDIS_ADDR", "127.0.0.1:6379"),
		RedisPass:         secretOrEnv("REDIS_PASSWORD"),
		RedisPrefix:       envOr("REDIS_PREFIX", "_dns:"),
		Zone:              envOr("DNS_ZONE", "srv.bartsch.red."),
		Hostname:          envOr("HOSTNAME", hostname),
		TTL:               envInt("DNS_TTL", 30),
		Network:           stripQuotes(os.Getenv("DOCKER_NETWORK")),
		StaticRecordsFile: stripQuotes(os.Getenv("STATIC_RECORDS_FILE")),
	}
}

// secretOrEnv resolves a value from KEY or KEY_FILE.
// Both being set at once is a fatal misconfiguration.
func secretOrEnv(key string) string {
	fileKey := key + "_FILE"
	filePath := stripQuotes(os.Getenv(fileKey))
	plainVal := stripQuotes(os.Getenv(key))
	if filePath != "" && plainVal != "" {
		log.Fatalf("CONFIG ERROR: both %s and %s are set — use only one", key, fileKey)
	}
	if filePath != "" {
		b, err := os.ReadFile(filePath)
		if err != nil {
			log.Fatalf("CONFIG ERROR: cannot read %s=%q: %v", fileKey, filePath, err)
		}
		val := strings.TrimSpace(string(b))
		if val == "" {
			log.Fatalf("CONFIG ERROR: secret file %q is empty", filePath)
		}
		log.Printf("CONFIG: %s loaded from file %s", key, filePath)
		return val
	}
	return plainVal
}

// ---------------------------------------------------------------------------
// Docker REST client — no external dependencies, Linux only
// ---------------------------------------------------------------------------

type dockerClient struct {
	hc  *http.Client
	cfg Config
}

func newDockerClient(cfg Config) *dockerClient {
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", cfg.DockerSocket)
			},
		},
	}
	return &dockerClient{hc: hc, cfg: cfg}
}

func (d *dockerClient) get(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("docker API %s: %d %s", path, resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ContainerSummary is the subset of fields we need from /containers/json.
type ContainerSummary struct {
	ID   string `json:"Id"`
	Name string `json:"Names"`
}

// ContainerInfo is the subset of fields we need from /containers/{id}/json.
type ContainerInfo struct {
	Name            string `json:"Name"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// listContainers returns running container IDs.
func (d *dockerClient) listContainers(ctx context.Context) ([]string, error) {
	var containers []struct {
		ID string `json:"Id"`
	}
	if err := d.get(ctx, "/containers/json", &containers); err != nil {
		return nil, err
	}
	ids := make([]string, len(containers))
	for i, c := range containers {
		ids[i] = c.ID
	}
	return ids, nil
}

// inspectContainer returns name and preferred IP for a container.
func (d *dockerClient) inspectContainer(ctx context.Context, id string) (name, ip string, err error) {
	var info ContainerInfo
	if err = d.get(ctx, "/containers/"+id+"/json", &info); err != nil {
		return
	}
	name = strings.TrimPrefix(info.Name, "/")
	nets := info.NetworkSettings.Networks
	if d.cfg.Network != "" {
		if n, ok := nets[d.cfg.Network]; ok && n.IPAddress != "" {
			return name, n.IPAddress, nil
		}
	}
	for _, n := range nets {
		if n.IPAddress != "" {
			return name, n.IPAddress, nil
		}
	}
	err = fmt.Errorf("container %s has no IP", name)
	return
}

// DockerEvent is the minimal shape of a Docker event message.
type DockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID string `json:"ID"`
	} `json:"Actor"`
}

// streamEvents opens /events and sends each parsed event to ch.
// Reconnects automatically on error.
func (d *dockerClient) streamEvents(ctx context.Context, ch chan<- DockerEvent) {
	filter := `?filters={"type":["container"],"event":["start","stop","die","destroy"]}`
	for {
		if ctx.Err() != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/events"+filter, nil)
		if err != nil {
			log.Printf("events: build request: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		resp, err := d.hc.Do(req)
		if err != nil {
			log.Printf("events: connect: %v — retrying in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("Listening for Docker events...")
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var ev DockerEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err == nil {
				ch <- ev
			}
		}
		resp.Body.Close()
		if ctx.Err() != nil {
			return
		}
		log.Printf("events: stream closed — reconnecting in 5s")
		time.Sleep(5 * time.Second)
	}
}

// ---------------------------------------------------------------------------
// DNS record structures (coredns-redis JSON format)
// ---------------------------------------------------------------------------

type aRecord struct {
	TTL int    `json:"ttl"`
	IP  string `json:"ip"`
}

type dnsRecord struct {
	A []aRecord `json:"a,omitempty"`
}

func (r dnsRecord) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// ---------------------------------------------------------------------------
// Syncer
// ---------------------------------------------------------------------------

type Syncer struct {
	cfg    Config
	docker *dockerClient
	rdb    *redis.Client
}

func NewSyncer(cfg Config) *Syncer {
	return &Syncer{
		cfg:    cfg,
		docker: newDockerClient(cfg),
		rdb: redis.NewClient(&redis.Options{
			Addr:         cfg.RedisAddr,
			Password:     cfg.RedisPass,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
		}),
	}
}

func (s *Syncer) zoneKey() string     { return s.cfg.RedisPrefix + s.cfg.Zone }
func (s *Syncer) trackingKey() string { return s.cfg.RedisPrefix + "tracking:" + s.cfg.Hostname }

func (s *Syncer) register(ctx context.Context, id string) {
	name, ip, err := s.docker.inspectContainer(ctx, id)
	if err != nil {
		log.Printf("SKIP register %.12s: %v", id, err)
		return
	}
	rec := dnsRecord{A: []aRecord{{TTL: s.cfg.TTL, IP: ip}}}
	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, s.zoneKey(), name, rec.JSON())
	pipe.SAdd(ctx, s.trackingKey(), name)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("ERROR register %s -> %s: %v", name, ip, err)
		return
	}
	log.Printf("REGISTER %s -> %s", name, ip)
}

func (s *Syncer) deregister(ctx context.Context, id string) {
	// best-effort inspect; container may already be gone
	name, _, _ := s.docker.inspectContainer(ctx, id)
	if name == "" {
		log.Printf("SKIP deregister %.12s: cannot determine name", id)
		return
	}
	pipe := s.rdb.Pipeline()
	pipe.HDel(ctx, s.zoneKey(), name)
	pipe.SRem(ctx, s.trackingKey(), name)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("ERROR deregister %s: %v", name, err)
		return
	}
	log.Printf("DEREGISTER %s", name)
}

func (s *Syncer) ensureSOA(ctx context.Context) {
	soa := `{"soa":{"ttl":300,"mbox":"hostmaster.bartsch.red.","ns":"ns1.bartsch.red.","refresh":300,"retry":60,"expire":3600}}`
	set, err := s.rdb.HSetNX(ctx, s.zoneKey(), "@", soa).Result()
	if err != nil {
		log.Printf("ERROR ensureSOA: %v", err)
		return
	}
	if set {
		log.Printf("SOA created for zone %s", s.cfg.Zone)
	}
}

// readStaticRecords parses a YAML or JSON file into a map of DNS name → raw JSON.
// Format: each top-level key is a DNS name (e.g. "@", "ns1"); its value is the
// coredns-redis record object (e.g. {"soa":{...},"ns":[...]}).
func readStaticRecords(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("parse YAML: %w", err)
		}
	default: // .json or unknown — try JSON first, fall back to YAML
		if err := json.Unmarshal(data, &raw); err != nil {
			if err2 := yaml.Unmarshal(data, &raw); err2 != nil {
				return nil, fmt.Errorf("cannot parse as JSON (%v) or YAML (%v)", err, err2)
			}
		}
	}
	result := make(map[string]json.RawMessage, len(raw))
	for name, val := range raw {
		b, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("marshal record %q: %w", name, err)
		}
		result[name] = b
	}
	return result, nil
}

func (s *Syncer) watchStaticRecords(ctx context.Context) {
	if s.cfg.StaticRecordsFile == "" {
		return
	}
	info, err := os.Stat(s.cfg.StaticRecordsFile)
	if err != nil {
		log.Printf("WARN watchStaticRecords: cannot stat %s: %v", s.cfg.StaticRecordsFile, err)
		return
	}
	lastMod := info.ModTime()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Stat(s.cfg.StaticRecordsFile)
			if err != nil {
				log.Printf("WARN watchStaticRecords: cannot stat %s: %v", s.cfg.StaticRecordsFile, err)
				continue
			}
			if info.ModTime().After(lastMod) {
				lastMod = info.ModTime()
				log.Printf("Static records file changed — re-applying")
				s.applyStaticRecords(ctx)
			}
		}
	}
}

func (s *Syncer) applyStaticRecords(ctx context.Context) {
	if s.cfg.StaticRecordsFile == "" {
		return
	}
	records, err := readStaticRecords(s.cfg.StaticRecordsFile)
	if err != nil {
		log.Printf("ERROR loading static records from %s: %v", s.cfg.StaticRecordsFile, err)
		return
	}
	pipe := s.rdb.Pipeline()
	for name, val := range records {
		log.Printf("Static record: %s → %s", name, val)
		pipe.HSet(ctx, s.zoneKey(), name, string(val))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("ERROR applying static records: %v", err)
		return
	}
	log.Printf("Applied %d static record(s) from %s", len(records), s.cfg.StaticRecordsFile)
}

func (s *Syncer) fullSync(ctx context.Context) {
	log.Printf("Full sync starting [host=%s]", s.cfg.Hostname)

	// clean stale records from previous run
	old, err := s.rdb.SMembers(ctx, s.trackingKey()).Result()
	if err == nil && len(old) > 0 {
		pipe := s.rdb.Pipeline()
		for _, e := range old {
			pipe.HDel(ctx, s.zoneKey(), e)
		}
		pipe.Del(ctx, s.trackingKey())
		if _, err := pipe.Exec(ctx); err != nil {
			log.Printf("WARN stale cleanup: %v", err)
		} else {
			log.Printf("Cleaned %d stale entries", len(old))
		}
	}

	ids, err := s.docker.listContainers(ctx)
	if err != nil {
		log.Printf("ERROR ContainerList: %v", err)
		return
	}
	for _, id := range ids {
		s.register(ctx, id)
	}
	s.applyStaticRecords(ctx)
	log.Printf("Full sync complete — %d containers", len(ids))
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	cfg := loadConfig()
	log.Printf("dns-sync %s starting | host=%s zone=%s redis=%s socket=%s",
		versionString(), cfg.Hostname, cfg.Zone, cfg.RedisAddr, cfg.DockerSocket)

	syncer := NewSyncer(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// wait for KeyDB
	for {
		if err := syncer.rdb.Ping(ctx).Err(); err != nil {
			log.Printf("Waiting for KeyDB (%s): %v", cfg.RedisAddr, err)
			time.Sleep(3 * time.Second)
			continue
		}
		break
	}
	log.Printf("KeyDB connected at %s", cfg.RedisAddr)

	if syncer.cfg.StaticRecordsFile != "" {
		syncer.applyStaticRecords(ctx)
	} else {
		syncer.ensureSOA(ctx)
	}
	syncer.fullSync(ctx)

	go syncer.watchStaticRecords(ctx)

	// event loop
	evCh := make(chan DockerEvent, 32)
	go syncer.docker.streamEvents(ctx, evCh)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-evCh:
				switch ev.Action {
				case "start":
					syncer.register(ctx, ev.Actor.ID)
				case "stop", "die", "destroy":
					syncer.deregister(ctx, ev.Actor.ID)
				}
			}
		}
	}()

	// periodic safety re-sync
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				log.Printf("Periodic re-sync")
				syncer.fullSync(ctx)
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	log.Printf("Shutting down")
	cancel()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func stripQuotes(s string) string {
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func envOr(key, fallback string) string {
	if v := stripQuotes(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := stripQuotes(os.Getenv(key)); v != "" {
		var i int
		if _, err := fmt.Sscanf(v, "%d", &i); err == nil {
			return i
		}
	}
	return fallback
}
