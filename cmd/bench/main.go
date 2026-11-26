//go:build linux

// Command bench runs reproducible HTTP/S3 performance benchmarks against a
// managed GoStore server process and records latency, throughput, errors,
// CPU, memory, open FDs, disk usage, and environment metadata.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	benchAccessKey = "gostore-bench-access"
	benchSecretKey = "gostore-bench-secret"
	benchRegion    = "us-east-1"
	unsignedHash   = "UNSIGNED-PAYLOAD"
)

type envInfo struct {
	OS         string `json:"os"`
	Kernel     string `json:"kernel"`
	CPUModel   string `json:"cpu_model"`
	NumCPU     int    `json:"num_cpu"`
	MemTotalMB int64  `json:"mem_total_mb"`
	GoVersion  string `json:"go_version"`
	DataDir    string `json:"data_dir"`
	Filesystem string `json:"filesystem"`
	CacheNote  string `json:"cache_note"`
}

type scenarioResult struct {
	Scenario    string  `json:"scenario"`
	Concurrency int     `json:"concurrency"`
	ObjectBytes int64   `json:"object_bytes"`
	Ops         int64   `json:"ops"`
	Errors      int64   `json:"errors"`
	DurationSec float64 `json:"duration_sec"`
	OpsPerSec   float64 `json:"ops_per_sec"`
	MiBPerSec   float64 `json:"mib_per_sec"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	P99Ms       float64 `json:"p99_ms"`
	CPUPct      float64 `json:"cpu_pct"`
	PeakRSSMiB  float64 `json:"peak_rss_mib"`
	PeakFDs     int     `json:"peak_fds"`
	DiskMiB     float64 `json:"disk_mib"`
}

type benchReport struct {
	Command   string           `json:"command"`
	Timestamp string           `json:"timestamp"`
	Env       envInfo          `json:"env"`
	Results   []scenarioResult `json:"results"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "bench error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	serverBinFlag := fs.String("server-bin", "", "path to prebuilt gostore server binary (builds ./cmd/server if empty)")
	dataDirFlag := fs.String("data-dir", "", "base directory for benchmark data (creates temporary dir inside; default OS temp)")
	durationFlag := fs.Duration("duration", 3*time.Second, "measurement duration per (scenario, concurrency) cell")
	concFlag := fs.String("concurrency", "1,8,32", "comma-separated concurrency levels for 4 KiB, 1 MiB, mixed, and list scenarios")
	largeConcFlag := fs.String("large-concurrency", "1,4", "comma-separated concurrency levels for 32 MiB large transfer scenarios")
	scenariosFlag := fs.String("scenarios", "all", "comma-separated scenarios or 'all' (put_4k,get_4k,put_1m,get_1m,put_32m,get_32m,mixed_rw,list_prefix)")
	pprofDirFlag := fs.String("pprof-dir", "", "optional directory to save CPU and heap profiles")
	pprofTargetFlag := fs.String("pprof-scenario", "put_4k@8", "target <scenario>@<concurrency> to capture pprof during")
	jsonFlag := fs.String("json", "", "optional path to write JSON report")
	keepDataFlag := fs.Bool("keep-data", false, "keep benchmark working directory after exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	concs, err := parseIntList(*concFlag)
	if err != nil {
		return fmt.Errorf("invalid -concurrency: %w", err)
	}
	largeConcs, err := parseIntList(*largeConcFlag)
	if err != nil {
		return fmt.Errorf("invalid -large-concurrency: %w", err)
	}
	wantScenarios := parseScenarioSet(*scenariosFlag)

	workRoot, err := os.MkdirTemp(*dataDirFlag, "gostore-bench-*")
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	if !*keepDataFlag {
		defer os.RemoveAll(workRoot)
	}

	serverBin := *serverBinFlag
	if serverBin == "" {
		serverBin = filepath.Join(workRoot, "gostore-server")
		buildCmd := exec.Command("go", "build", "-o", serverBin, "./cmd/server")
		if out, err := buildCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build ./cmd/server: %w\n%s", err, out)
		}
	}

	dataDir := filepath.Join(workRoot, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}

	addr, err := freeLocalAddr()
	if err != nil {
		return err
	}
	var pprofAddr string
	if *pprofDirFlag != "" {
		if err := os.MkdirAll(*pprofDirFlag, 0o755); err != nil {
			return err
		}
		pprofAddr, err = freeLocalAddr()
		if err != nil {
			return err
		}
	}

	logPath := filepath.Join(workRoot, "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	srvCmd := exec.Command(serverBin)
	srvCmd.Env = append(os.Environ(),
		"GOSTORE_ADDR="+addr,
		"GOSTORE_DATA_DIR="+dataDir,
		"GOSTORE_ACCESS_KEY="+benchAccessKey,
		"GOSTORE_SECRET_KEY="+benchSecretKey,
		"GOSTORE_REGION="+benchRegion,
		"GOSTORE_MIN_FREE_SPACE=104857600",
		"GOSTORE_PPROF_ADDR="+pprofAddr,
	)
	srvCmd.Stdout = logFile
	srvCmd.Stderr = logFile
	if err := srvCmd.Start(); err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	srvDone := make(chan error, 1)
	go func() { srvDone <- srvCmd.Wait() }()
	defer func() {
		if srvCmd.Process != nil {
			_ = srvCmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-srvDone:
			case <-time.After(10 * time.Second):
				_ = srvCmd.Process.Kill()
				<-srvDone
			}
		}
	}()

	client := newBenchClient("http://"+addr, benchAccessKey, benchSecretKey, benchRegion)
	if err := waitReady(client.http, "http://"+addr+"/-/readyz", srvDone); err != nil {
		logBytes, _ := os.ReadFile(logPath)
		return fmt.Errorf("server failed readiness: %w\n%s", err, string(logBytes))
	}

	env := collectEnv(dataDir)
	report := benchReport{
		Command:   "go run ./cmd/bench " + strings.Join(args, " "),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Env:       env,
	}
	printEnvBanner(env)

	var (
		seeded4k    bool
		seeded1m    bool
		seeded32m   bool
		seededMixed bool
		seededList  bool
	)

	payload4k := makePayload(4 * 1024)
	payload64k := makePayload(64 * 1024)
	payload1m := makePayload(1 << 20)
	payload32m := makePayload(32 << 20)

	type spec struct {
		name  string
		size  int64
		concs []int
		setup func() error
		op    func(workerID, seq int) (int64, error)
		clean func()
	}

	specs := []spec{
		{
			name:  "put_4k",
			size:  int64(len(payload4k)),
			concs: concs,
			setup: func() error { return client.createBucket("b-put4k") },
			op: func(w, seq int) (int64, error) {
				key := fmt.Sprintf("k-%d-%d", w, seq)
				return int64(len(payload4k)), client.putObject("b-put4k", key, payload4k)
			},
		},
		{
			name:  "get_4k",
			size:  int64(len(payload4k)),
			concs: concs,
			setup: func() error {
				if seeded4k {
					return nil
				}
				if err := client.createBucket("b-get4k"); err != nil {
					return err
				}
				for i := 0; i < 128; i++ {
					if err := client.putObject("b-get4k", fmt.Sprintf("k-%03d", i), payload4k); err != nil {
						return err
					}
				}
				seeded4k = true
				return nil
			},
			op: func(w, seq int) (int64, error) {
				idx := (w*131 + seq) % 128
				return client.getObject("b-get4k", fmt.Sprintf("k-%03d", idx))
			},
		},
		{
			name:  "put_1m",
			size:  int64(len(payload1m)),
			concs: concs,
			setup: func() error { return client.createBucketIfNeeded("b-put1m") },
			op: func(w, seq int) (int64, error) {
				// Bound retained disk footprint by rotating over 128 keys.
				slot := (w*17 + seq) % 128
				return int64(len(payload1m)), client.putObject("b-put1m", fmt.Sprintf("k-%03d", slot), payload1m)
			},
		},
		{
			name:  "get_1m",
			size:  int64(len(payload1m)),
			concs: concs,
			setup: func() error {
				if seeded1m {
					return nil
				}
				if err := client.createBucket("b-get1m"); err != nil {
					return err
				}
				for i := 0; i < 32; i++ {
					if err := client.putObject("b-get1m", fmt.Sprintf("k-%02d", i), payload1m); err != nil {
						return err
					}
				}
				seeded1m = true
				return nil
			},
			op: func(w, seq int) (int64, error) {
				idx := (w*31 + seq) % 32
				return client.getObject("b-get1m", fmt.Sprintf("k-%02d", idx))
			},
		},
		{
			name:  "put_32m",
			size:  int64(len(payload32m)),
			concs: largeConcs,
			setup: func() error { return client.createBucketIfNeeded("b-put32m") },
			op: func(w, seq int) (int64, error) {
				slot := (w + seq) % 8
				return int64(len(payload32m)), client.putObject("b-put32m", fmt.Sprintf("big-%02d", slot), payload32m)
			},
		},
		{
			name:  "get_32m",
			size:  int64(len(payload32m)),
			concs: largeConcs,
			setup: func() error {
				if seeded32m {
					return nil
				}
				if err := client.createBucket("b-get32m"); err != nil {
					return err
				}
				for i := 0; i < 4; i++ {
					if err := client.putObject("b-get32m", fmt.Sprintf("big-%02d", i), payload32m); err != nil {
						return err
					}
				}
				seeded32m = true
				return nil
			},
			op: func(w, seq int) (int64, error) {
				idx := (w + seq) % 4
				return client.getObject("b-get32m", fmt.Sprintf("big-%02d", idx))
			},
		},
		{
			name:  "mixed_rw",
			size:  int64(len(payload64k)),
			concs: concs,
			setup: func() error {
				if seededMixed {
					return nil
				}
				if err := client.createBucket("b-mixed"); err != nil {
					return err
				}
				for i := 0; i < 128; i++ {
					if err := client.putObject("b-mixed", fmt.Sprintf("m-%03d", i), payload64k); err != nil {
						return err
					}
				}
				seededMixed = true
				return nil
			},
			op: func(w, seq int) (int64, error) {
				idx := (w*131 + seq) % 128
				key := fmt.Sprintf("m-%03d", idx)
				if (seq+w)%4 == 0 {
					return int64(len(payload64k)), client.putObject("b-mixed", key, payload64k)
				}
				return client.getObject("b-mixed", key)
			},
		},
		{
			name:  "list_prefix",
			size:  0,
			concs: concs,
			setup: func() error {
				if seededList {
					return nil
				}
				if err := client.createBucket("b-list"); err != nil {
					return err
				}
				for t := 0; t < 10; t++ {
					for d := 0; d < 10; d++ {
						for i := 0; i < 20; i++ {
							key := fmt.Sprintf("tenant-%02d/logs/day-%02d/entry-%03d.json", t, d, i)
							if err := client.putObject("b-list", key, payload4k[:128]); err != nil {
								return err
							}
						}
					}
				}
				seededList = true
				return nil
			},
			op: func(w, seq int) (int64, error) {
				q := "list-type=2&prefix=tenant-03/logs/day-05/&max-keys=100"
				if seq%2 == 1 {
					q = "list-type=2&prefix=tenant-03/logs/&delimiter=/&max-keys=100"
				}
				return client.listObjects("b-list", q)
			},
		},
	}

	fmt.Printf("\n%-12s %4s %8s %10s %10s %7s %8s %8s %8s %8s %9s %8s %9s\n",
		"Scenario", "Conc", "Ops", "Ops/s", "MiB/s", "Errors", "p50(ms)", "p95(ms)", "p99(ms)", "CPU(%)", "RSS(MiB)", "PeakFDs", "Disk(MiB)")
	fmt.Println(strings.Repeat("-", 122))

	pid := srvCmd.Process.Pid
	for _, sp := range specs {
		if len(wantScenarios) > 0 && !wantScenarios[sp.name] {
			continue
		}
		if err := sp.setup(); err != nil {
			return fmt.Errorf("setup %s: %w", sp.name, err)
		}
		for _, c := range sp.concs {
			tag := fmt.Sprintf("%s@%d", sp.name, c)
			var pprofStop chan struct{}
			var pprofWG sync.WaitGroup
			if *pprofDirFlag != "" && *pprofTargetFlag == tag {
				pprofStop = make(chan struct{})
				pprofWG.Add(1)
				go func() {
					defer pprofWG.Done()
					captureProfiles(pprofAddr, *pprofDirFlag, tag, *durationFlag, pprofStop)
				}()
			}

			res, err := runCell(sp.name, sp.size, c, *durationFlag, pid, dataDir, sp.op)
			if pprofStop != nil {
				close(pprofStop)
				pprofWG.Wait()
			}
			if err != nil {
				return fmt.Errorf("run %s: %w", tag, err)
			}
			report.Results = append(report.Results, res)
			fmt.Printf("%-12s %4d %8d %10.1f %10.2f %7d %8.2f %8.2f %8.2f %8.1f %9.1f %8d %9.1f\n",
				res.Scenario, res.Concurrency, res.Ops, res.OpsPerSec, res.MiBPerSec, res.Errors,
				res.P50Ms, res.P95Ms, res.P99Ms, res.CPUPct, res.PeakRSSMiB, res.PeakFDs, res.DiskMiB)
		}
		if sp.clean != nil {
			sp.clean()
		}
	}

	if *jsonFlag != "" {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*jsonFlag, raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func runCell(
	name string,
	objBytes int64,
	concurrency int,
	duration time.Duration,
	pid int,
	dataDir string,
	op func(workerID, seq int) (int64, error),
) (scenarioResult, error) {
	// Warm one operation per worker so connections and OS page cache settle.
	var warmWG sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		warmWG.Add(1)
		go func(id int) {
			defer warmWG.Done()
			_, _ = op(id, 0)
		}(w)
	}
	warmWG.Wait()

	cpuStart, err := readProcCPUTicks(pid)
	if err != nil {
		return scenarioResult{}, err
	}

	stopSample := make(chan struct{})
	var peakRSSKB int64
	var peakFDs int
	var sampleMu sync.Mutex
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		sample := func() {
			rss, _ := readProcRSSKB(pid)
			fds, _ := countProcFDs(pid)
			sampleMu.Lock()
			if rss > peakRSSKB {
				peakRSSKB = rss
			}
			if fds > peakFDs {
				peakFDs = fds
			}
			sampleMu.Unlock()
		}
		sample()
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopSample:
				sample()
				return
			case <-t.C:
				sample()
			}
		}
	}()

	minOpsPerWorker := 1
	if objBytes >= 32<<20 {
		minOpsPerWorker = 2
	}

	var totalBytes atomic.Int64
	var errCount atomic.Int64
	workerLatencies := make([][]float64, concurrency)

	deadline := time.Now().Add(duration)
	start := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			lats := make([]float64, 0, 256)
			seq := 1
			for seq <= minOpsPerWorker || time.Now().Before(deadline) {
				t0 := time.Now()
				n, opErr := op(id, seq)
				lats = append(lats, float64(time.Since(t0).Microseconds())/1000.0)
				if opErr != nil {
					errCount.Add(1)
				} else {
					totalBytes.Add(n)
				}
				seq++
			}
			workerLatencies[id] = lats
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)

	close(stopSample)
	sampleWG.Wait()

	cpuEnd, err := readProcCPUTicks(pid)
	if err != nil {
		return scenarioResult{}, err
	}

	allLats := make([]float64, 0, 1024)
	for _, wl := range workerLatencies {
		allLats = append(allLats, wl...)
	}
	slices.Sort(allLats)

	diskBytes, err := dirSize(dataDir)
	if err != nil {
		return scenarioResult{}, err
	}

	ops := int64(len(allLats))
	secs := elapsed.Seconds()
	cpuSecs := float64(cpuEnd-cpuStart) / 100.0
	return scenarioResult{
		Scenario:    name,
		Concurrency: concurrency,
		ObjectBytes: objBytes,
		Ops:         ops,
		Errors:      errCount.Load(),
		DurationSec: secs,
		OpsPerSec:   float64(ops) / secs,
		MiBPerSec:   (float64(totalBytes.Load()) / (1024 * 1024)) / secs,
		P50Ms:       percentile(allLats, 0.50),
		P95Ms:       percentile(allLats, 0.95),
		P99Ms:       percentile(allLats, 0.99),
		CPUPct:      (cpuSecs / secs) * 100.0,
		PeakRSSMiB:  float64(peakRSSKB) / 1024.0,
		PeakFDs:     peakFDs,
		DiskMiB:     float64(diskBytes) / (1024 * 1024),
	}, nil
}

func captureProfiles(pprofAddr, outDir, tag string, dur time.Duration, _ <-chan struct{}) {
	secs := int(dur.Seconds())
	if secs < 2 {
		secs = 2
	}
	// Let warm-up finish briefly, then sample CPU profile during active load.
	time.Sleep(300 * time.Millisecond)
	client := &http.Client{Timeout: dur + 10*time.Second}
	safeTag := strings.ReplaceAll(tag, "@", "_")
	if resp, err := client.Get(fmt.Sprintf("http://%s/debug/pprof/profile?seconds=%d", pprofAddr, secs)); err == nil {
		if body, err := io.ReadAll(resp.Body); err == nil && resp.StatusCode == http.StatusOK {
			_ = os.WriteFile(filepath.Join(outDir, safeTag+"_cpu.pb.gz"), body, 0o644)
		}
		_ = resp.Body.Close()
	}
	if resp, err := client.Get(fmt.Sprintf("http://%s/debug/pprof/heap", pprofAddr)); err == nil {
		if body, err := io.ReadAll(resp.Body); err == nil && resp.StatusCode == http.StatusOK {
			_ = os.WriteFile(filepath.Join(outDir, safeTag+"_heap.pb.gz"), body, 0o644)
		}
		_ = resp.Body.Close()
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func readProcCPUTicks(pid int) (uint64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	line := string(raw)
	rparen := strings.LastIndexByte(line, ')')
	if rparen < 0 || rparen+2 >= len(line) {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(line[rparen+2:])
	if len(fields) <= 12 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat field count", pid)
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return utime + stime, nil
}

func readProcRSSKB(pid int) (int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			fields := strings.Fields(rest)
			if len(fields) >= 1 {
				return strconv.ParseInt(fields[0], 10, 64)
			}
		}
	}
	return 0, nil
}

func countProcFDs(pid int) (int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func collectEnv(dataDir string) envInfo {
	info := envInfo{
		OS:        runtime.GOOS + "/" + runtime.GOARCH,
		NumCPU:    runtime.NumCPU(),
		GoVersion: runtime.Version(),
		DataDir:   dataDir,
		CacheNote: "OS page cache warm (1-op warmup per worker; explicit fsync/dirsync enabled)",
	}
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		info.Kernel = strings.TrimSpace(string(out))
	}
	if raw, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if _, v, ok := strings.Cut(line, ":"); ok && strings.HasPrefix(line, "model name") {
				info.CPUModel = strings.TrimSpace(v)
				break
			}
		}
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
				if fields := strings.Fields(rest); len(fields) >= 1 {
					if kb, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
						info.MemTotalMB = kb / 1024
					}
				}
				break
			}
		}
	}
	info.Filesystem = detectFS(dataDir)
	return info
}

func detectFS(path string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return "unknown"
	}
	switch uint64(st.Type) {
	case 0xef53:
		return "ext4"
	case 0x01021994:
		return "tmpfs"
	case 0x58465342:
		return "xfs"
	case 0x9123683e:
		return "btrfs"
	case 0x01021997:
		return "9p/drvfs"
	default:
		return fmt.Sprintf("0x%x", st.Type)
	}
}

func printEnvBanner(env envInfo) {
	fmt.Printf("Environment: %s | Kernel: %s | CPU: %s (%d vCPUs) | RAM: %d MiB | Go: %s | FS: %s (%s)\n",
		env.OS, env.Kernel, env.CPUModel, env.NumCPU, env.MemTotalMB, env.GoVersion, env.Filesystem, env.CacheNote)
}

func freeLocalAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr, nil
}

func waitReady(c *http.Client, readyURL string, srvDone <-chan error) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-srvDone:
			return fmt.Errorf("server exited early: %w", err)
		default:
		}
		resp, err := c.Get(readyURL)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("timed out waiting for /-/readyz")
}

func parseIntList(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("bad integer %q", p)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, errors.New("empty list")
	}
	return out, nil
}

func parseScenarioSet(raw string) map[string]bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "all" {
		return nil
	}
	out := map[string]bool{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out[p] = true
		}
	}
	return out
}

func makePayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

type benchClient struct {
	baseURL   string
	accessKey string
	secretKey string
	region    string
	http      *http.Client

	keyMu   sync.Mutex
	keyDate string
	signKey []byte
}

func newBenchClient(baseURL, accessKey, secretKey, region string) *benchClient {
	return &benchClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		http: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        512,
				MaxIdleConnsPerHost: 512,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (c *benchClient) signingKey(date string) []byte {
	c.keyMu.Lock()
	defer c.keyMu.Unlock()
	if c.keyDate == date && len(c.signKey) > 0 {
		return c.signKey
	}
	kDate := hmacSHA256([]byte("AWS4"+c.secretKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(c.region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	c.signKey = hmacSHA256(kService, []byte("aws4_request"))
	c.keyDate = date
	return c.signKey
}

func (c *benchClient) sign(req *http.Request) error {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	host := req.URL.Host
	req.Host = host

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", unsignedHash)

	canonURI := benchCanonURI(req.URL.EscapedPath())
	canonQuery := benchCanonQuery(req.URL.RawQuery)
	canonHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", host, unsignedHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonReq := req.Method + "\n" + canonURI + "\n" + canonQuery + "\n" + canonHeaders + "\n" + signedHeaders + "\n" + unsignedHash

	sum := sha256.Sum256([]byte(canonReq))
	scope := date + "/" + c.region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	sig := hex.EncodeToString(hmacSHA256(c.signingKey(date), []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKey, scope, signedHeaders, sig))
	return nil
}

func (c *benchClient) createBucket(name string) error {
	req, err := http.NewRequest(http.MethodPut, c.baseURL+"/"+name, nil)
	if err != nil {
		return err
	}
	if err := c.sign(req); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("create bucket %s: status %d", name, resp.StatusCode)
	}
	return nil
}

func (c *benchClient) createBucketIfNeeded(name string) error {
	req, err := http.NewRequest(http.MethodPut, c.baseURL+"/"+name, nil)
	if err != nil {
		return err
	}
	if err := c.sign(req); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("create bucket %s: status %d", name, resp.StatusCode)
	}
	return nil
}

func (c *benchClient) putObject(bucket, key string, data []byte) error {
	req, err := http.NewRequest(http.MethodPut, c.baseURL+"/"+bucket+"/"+key, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	if err := c.sign(req); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("put %s/%s: status %d", bucket, key, resp.StatusCode)
	}
	return nil
}

func (c *benchClient) getObject(bucket, key string) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/"+bucket+"/"+key, nil)
	if err != nil {
		return 0, err
	}
	if err := c.sign(req); err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("get %s/%s: status %d", bucket, key, resp.StatusCode)
	}
	return n, nil
}

func (c *benchClient) listObjects(bucket, rawQuery string) (int64, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+"/"+bucket+"?"+rawQuery, nil)
	if err != nil {
		return 0, err
	}
	if err := c.sign(req); err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("list %s: status %d", bucket, resp.StatusCode)
	}
	return n, nil
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func benchCanonURI(rawPath string) string {
	if rawPath == "" {
		return "/"
	}
	segs := strings.Split(rawPath, "/")
	for i, seg := range segs {
		u, _ := url.PathUnescape(seg)
		segs[i] = awsEncode(u)
	}
	return strings.Join(segs, "/")
}

func benchCanonQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	type pair struct{ k, v string }
	parts := strings.Split(rawQuery, "&")
	pairs := make([]pair, 0, len(parts))
	for _, p := range parts {
		kRaw, vRaw, _ := strings.Cut(p, "=")
		k, _ := url.QueryUnescape(kRaw)
		v, _ := url.QueryUnescape(vRaw)
		pairs = append(pairs, pair{k: awsEncode(k), v: awsEncode(v)})
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		if c := strings.Compare(a.k, b.k); c != 0 {
			return c
		}
		return strings.Compare(a.v, b.v)
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

func awsEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '~' || c == '.' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}
