package chbackup

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli"
	"golang.org/x/sync/semaphore"
	yaml "gopkg.in/yaml.v2"
)

const (
	// APITimeFormat - clickhouse compatibility time format
	APITimeFormat = "2006-01-02 15:04:05"
)

type APIServer struct {
	c       *cli.App
	config  Config
	lock    *semaphore.Weighted
	server  *http.Server
	restart chan struct{}
	status  *AsyncStatus
	metrics Metrics
	routes  []string
}

type AsyncStatus struct {
	commands []CommandInfo
	sync.RWMutex
}

type CommandInfo struct {
	Command    string `json:"command"`
	Status     string `json:"status"`
	Progress   string `json:"progress,omitempty"`
	Start      string `json:"start,omitempty"`
	Finish     string `json:"finish,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (status *AsyncStatus) start(command string) {
	status.Lock()
	defer status.Unlock()
	status.commands = append(status.commands, CommandInfo{
		Command: command,
		Start:   time.Now().Format(APITimeFormat),
		Status:  "in progress",
	})
}

func (status *AsyncStatus) stop(err error) {
	status.Lock()
	defer status.Unlock()
	n := len(status.commands) - 1
	s := "success"
	if err != nil {
		s = "error"
		status.commands[n].Error = err.Error()
	}
	status.commands[n].Status = s
	status.commands[n].Finish = time.Now().Format(APITimeFormat)
}

func (status *AsyncStatus) status() []CommandInfo {
	status.RLock()
	defer status.RUnlock()
	return status.commands
}

var (
	ErrAPILocked = errors.New("another operation is currently running")
)

// Server - expose CLI commands as REST API
func Server(c *cli.App, config Config) error {
	api := APIServer{
		c:       c,
		config:  config,
		lock:    semaphore.NewWeighted(1),
		restart: make(chan struct{}),
		status:  &AsyncStatus{},
	}
	api.metrics = setupMetrics()
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, os.Interrupt, syscall.SIGTERM)
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, os.Interrupt, syscall.SIGHUP)

	for {
		api.server = api.setupAPIServer(api.config)
		go func() {
			log.Printf("Starting API server on %s", api.config.API.ListenAddr)
			if err := api.server.ListenAndServe(); err != http.ErrServerClosed {
				log.Printf("error starting API server: %v", err)
				os.Exit(1)
			}
		}()
		select {
		case <-api.restart:
			log.Println("Reloading config and restarting API server")
			api.server.Close()
			continue
		case <-sighup:
			log.Println("Reloading config and restarting API server")
			api.server.Close()
			continue
		case <-sigterm:
			log.Println("Stopping API server")
			return api.server.Close()
		}
	}
}

// setupAPIServer - resister API routes
func (api *APIServer) setupAPIServer(config Config) *http.Server {
	r := mux.NewRouter()
	r.Use(api.basicAuthMidleware)
	r.HandleFunc("/", api.httpRootHandler).Methods("GET")

	r.HandleFunc("/backup/tables", api.httpTablesHandler).Methods("GET")
	r.HandleFunc("/backup/list", api.httpListHandler).Methods("GET")
	r.HandleFunc("/backup/create", api.httpCreateHandler).Methods("POST")
	r.HandleFunc("/backup/clean", api.httpCleanHandler).Methods("POST")
	r.HandleFunc("/backup/freeze", api.httpFreezeHandler).Methods("POST")
	r.HandleFunc("/backup/upload/{name}", api.httpUploadHandler).Methods("POST")
	r.HandleFunc("/backup/download/{name}", api.httpDownloadHandler).Methods("POST")
	r.HandleFunc("/backup/restore/{name}", api.httpRestoreHandler).Methods("POST")
	r.HandleFunc("/backup/delete/{where}/{name}", api.httpDeleteHandler).Methods("POST")
	r.HandleFunc("/backup/config/default", httpConfigDefaultHandler).Methods("GET")
	r.HandleFunc("/backup/config", api.httpConfigHandler).Methods("GET")
	r.HandleFunc("/backup/config", api.httpConfigUpdateHandler).Methods("POST")
	r.HandleFunc("/backup/status", api.httpBackupStatusHandler).Methods("GET")

	r.HandleFunc("/integration/actions", api.integrationBackupLog).Methods("GET")
	r.HandleFunc("/integration/list", api.httpListHandler).Methods("GET")

	r.HandleFunc("/integration/actions", api.integrationPost).Methods("POST")

	var routes []string
	r.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		t, err := route.GetPathTemplate()
		if err != nil {
			return err
		}
		routes = append(routes, t)
		return nil
	})
	api.routes = routes
	registerMetricsHandlers(r, config.API.EnableMetrics, config.API.EnablePprof)

	srv := &http.Server{
		Addr:    config.API.ListenAddr,
		Handler: r,
	}
	return srv
}

func (api *APIServer) basicAuthMidleware(next http.Handler) http.Handler {
	if api.config.API.Username == "" && api.config.API.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		query := r.URL.Query()
		log.Println("query", query)
		if u, exist := query["user"]; exist {
			user = u[0]
		}
		if p, exist := query["pass"]; exist {
			pass = p[0]
		}
		if (user != api.config.API.Username) || (pass != api.config.API.Password) {
			w.Header().Set("WWW-Authenticate", "Basic realm=\"Provide username and password\"")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("401 Unauthorized\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CREATE TABLE system.backup_actions (command String, start DateTime, finish DateTime, status String, error String) ENGINE=URL('http://127.0.0.1:7171/integration/actions?user=user&pass=pass', TSVWithNames)
// INSERT INTO system.backup_actions (command) VALUES ('create backup_name')
// INSERT INTO system.backup_actions (command) VALUES ('upload backup_name')
func (api *APIServer) integrationPost(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	lines := strings.Split(string(body), "\n")
	if len(lines) < 2 {
		http.Error(w, "use TSVWithNames format", http.StatusBadRequest)
		return
	}
	columns := strings.Split(lines[1], "\t")
	commands := strings.Split(columns[0], " ")
	log.Println(commands)

	switch commands[0] {
	case "create", "upload", "download":
		if locked := api.lock.TryAcquire(1); !locked {
			log.Println(ErrAPILocked)
			http.Error(w, ErrAPILocked.Error(), http.StatusLocked)
			return
		}
		defer api.lock.Release(1)
		start := time.Now()
		api.metrics.LastBackupStart.Set(float64(start.Unix()))
		defer api.metrics.LastBackupDuration.Set(float64(time.Since(start).Nanoseconds()))
		defer api.metrics.LastBackupEnd.Set(float64(time.Now().Unix()))

		go func() {
			api.status.start(columns[0])
			err := api.c.Run(append([]string{"clickhouse-backup"}, commands...))
			defer api.status.stop(err)
			if err != nil {
				api.metrics.FailedBackups.Inc()
				api.metrics.LastBackupSuccess.Set(0)
				log.Println(err)
				return
			}
		}()
		api.metrics.SuccessfulBackups.Inc()
		api.metrics.LastBackupSuccess.Set(1)
		fmt.Fprintln(w, "acknowledged")
		return
	case "delete", "freeze", "clean":
		if locked := api.lock.TryAcquire(1); !locked {
			log.Println(ErrAPILocked)
			http.Error(w, ErrAPILocked.Error(), http.StatusLocked)
			return
		}
		defer api.lock.Release(1)
		start := time.Now()
		api.metrics.LastBackupStart.Set(float64(start.Unix()))
		defer api.metrics.LastBackupDuration.Set(float64(time.Since(start).Nanoseconds()))
		defer api.metrics.LastBackupEnd.Set(float64(time.Now().Unix()))

		api.status.start(columns[0])
		err := api.c.Run(append([]string{"clickhouse-backup"}, commands...))
		defer api.status.stop(err)
		if err != nil {
			api.metrics.FailedBackups.Inc()
			api.metrics.LastBackupSuccess.Set(0)
			http.Error(w, err.Error(), http.StatusBadRequest)
			log.Println(err)
			return
		}
		api.metrics.SuccessfulBackups.Inc()
		api.metrics.LastBackupSuccess.Set(1)
		fmt.Fprintln(w, "OK")
		log.Println("OK")
		return
	default:
		http.Error(w, fmt.Sprintf("bad command '%s'", columns[0]), http.StatusBadRequest)
	}
}

// CREATE TABLE system.backup_list (name String, created DateTime, size Int64, location String) ENGINE=URL('http://127.0.0.1:7171/integration/list?user=user&pass=pass', TSVWithNames)
// ??? INSERT INTO system.backup_list (name,location) VALUES ('backup_name', 'remote') - upload backup
// ??? INSERT INTO system.backup_list (name) VALUES ('backup_name') - create backup
func (api *APIServer) integrationBackupLog(w http.ResponseWriter, r *http.Request) {
	commands := api.status.status()
	fmt.Fprintln(w, "command\tstart\tfinish\tstatus\terror")
	for _, c := range commands {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.Command, c.Start, c.Finish, c.Status, c.Error)
	}
}

// httpRootHandler - display API index
func (api *APIServer) httpRootHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")

	fmt.Fprintln(w, "Documentation: https://github.com/AlexAkulov/clickhouse-backup#api-configuration")
	for _, r := range api.routes {
		fmt.Fprintln(w, r)
	}
}

// httpConfigDefaultHandler - display the default config. Same as CLI: clickhouse-backup default-config
func httpConfigDefaultHandler(w http.ResponseWriter, r *http.Request) {
	defaultConfig := DefaultConfig()
	body, err := yaml.Marshal(defaultConfig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "default-config", err)
	}
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	fmt.Fprintln(w, string(body))
}

// httpConfigDefaultHandler - display the currently running config
func (api *APIServer) httpConfigHandler(w http.ResponseWriter, r *http.Request) {
	config := api.config
	config.ClickHouse.Password = "***"
	config.API.Password = "***"
	config.S3.SecretKey = "***"
	config.GCS.CredentialsJSON = "***"
	config.COS.SecretKey = "***"
	config.FTP.Password = "***"
	body, err := yaml.Marshal(&config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config", err)
	}
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	fmt.Fprintln(w, string(body))
}

// httpConfigDefaultHandler - update the currently running config
func (api *APIServer) httpConfigUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if locked := api.lock.TryAcquire(1); !locked {
		log.Println(ErrAPILocked)
		writeError(w, http.StatusServiceUnavailable, "update", ErrAPILocked)
		return
	}
	defer api.lock.Release(1)

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "update", fmt.Errorf("reading body error: %v", err))
		return
	}

	newConfig := DefaultConfig()
	if err := yaml.Unmarshal(body, &newConfig); err != nil {
		writeError(w, http.StatusBadRequest, "update", fmt.Errorf("error parsing new config: %v", err))
		return
	}

	if err := validateConfig(newConfig); err != nil {
		writeError(w, http.StatusBadRequest, "update", fmt.Errorf("error validating new config: %v", err))
		return
	}
	log.Printf("Applying new valid config")
	api.config = *newConfig
	api.restart <- struct{}{}
}

// httpTablesHandler - displaylist of tables
func (api *APIServer) httpTablesHandler(w http.ResponseWriter, r *http.Request) {
	tables, err := getTables(api.config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "tables", err)
		return
	}
	sendResponse(w, http.StatusOK, tables)
}

// httpTablesHandler - display list of all backups stored locally and remotely
func (api *APIServer) httpListHandler(w http.ResponseWriter, r *http.Request) {
	type backup struct {
		Name     string `json:"name"`
		Created  string `json:"created"`
		Size     int64  `json:"size,omitempty"`
		Location string `json:"location"`
	}
	backups := make([]backup, 0)
	localBackups, err := ListLocalBackups(api.config)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "list", err)
		return
	}

	for _, b := range localBackups {
		backups = append(backups, backup{
			Name:     b.Name,
			Created:  b.Date.Format(APITimeFormat),
			Location: "local",
		})
	}
	if api.config.General.RemoteStorage != "none" {
		remoteBackups, err := getRemoteBackups(api.config)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list", err)
			return
		}
		for _, b := range remoteBackups {
			backups = append(backups, backup{
				Name:     b.Name,
				Created:  b.Date.Format(APITimeFormat),
				Size:     b.Size,
				Location: "remote",
			})
		}
	}
	if r.URL.Path == "/backup/list" {
		sendResponse(w, http.StatusOK, &backups)
		return
	}
	fmt.Fprintln(w, "name\tcreated\tsize\tlocation")
	for _, b := range backups {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", b.Name, b.Created, b.Size, b.Location)
	}
}

// httpCreateHandler - create a backup
func (api *APIServer) httpCreateHandler(w http.ResponseWriter, r *http.Request) {
	if locked := api.lock.TryAcquire(1); !locked {
		log.Println(ErrAPILocked)
		writeError(w, http.StatusLocked, "create", ErrAPILocked)
		return
	}
	defer api.lock.Release(1)
	start := time.Now()
	api.metrics.LastBackupStart.Set(float64(start.Unix()))
	defer api.metrics.LastBackupDuration.Set(float64(time.Since(start).Nanoseconds()))
	defer api.metrics.LastBackupEnd.Set(float64(time.Now().Unix()))

	tablePattern := ""
	backupName := NewBackupName()

	query := r.URL.Query()
	if tp, exist := query["table"]; exist {
		tablePattern = tp[0]
	}
	if name, exist := query["name"]; exist {
		backupName = name[0]
	}

	go func() {
		api.status.start("create")
		err := CreateBackup(api.config, backupName, tablePattern)
		defer api.status.stop(err)
		if err != nil {
			api.metrics.FailedBackups.Inc()
			api.metrics.LastBackupSuccess.Set(0)
			log.Printf("CreateBackup error: %v", err)
			return
		}
	}()
	api.metrics.SuccessfulBackups.Inc()
	api.metrics.LastBackupSuccess.Set(1)
	sendResponse(w, http.StatusCreated, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
	}{
		Status:     "acknowledged",
		Operation:  "create",
		BackupName: backupName,
	})
}

// httpFreezeHandler - freeze tables
func (api *APIServer) httpFreezeHandler(w http.ResponseWriter, r *http.Request) {
	if locked := api.lock.TryAcquire(1); !locked {
		log.Println(ErrAPILocked)
		writeError(w, http.StatusLocked, "freeze", ErrAPILocked)
		return
	}
	defer api.lock.Release(1)
	api.status.start("freeze")

	query := r.URL.Query()
	tablePattern := ""
	if tp, exist := query["table"]; exist {
		tablePattern = tp[0]
	}
	if err := Freeze(api.config, tablePattern); err != nil {
		log.Printf("Freeze error: = %+v\n", err)
		writeError(w, http.StatusInternalServerError, "freeze", err)
		return
	}
	sendResponse(w, http.StatusOK, struct {
		Status    string `json:"status"`
		Operation string `json:"operation"`
	}{
		Status:    "success",
		Operation: "freeze",
	})
}

// httpCleanHandler - clean ./shadow directory
func (api *APIServer) httpCleanHandler(w http.ResponseWriter, r *http.Request) {
	if locked := api.lock.TryAcquire(1); !locked {
		log.Println(ErrAPILocked)
		writeError(w, http.StatusLocked, "clean", ErrAPILocked)
		return
	}
	defer api.lock.Release(1)
	api.status.start("clean")
	err := Clean(api.config)
	api.status.stop(err)
	if err != nil {
		log.Printf("Clean error: = %+v\n", err)
		writeError(w, http.StatusInternalServerError, "clean", err)
		return
	}
	sendResponse(w, http.StatusOK, struct {
		Status    string `json:"status"`
		Operation string `json:"operation"`
	}{
		Status:    "success",
		Operation: "clean",
	})
}

// httpUploadHandler - upload a backup to remote storage
func (api *APIServer) httpUploadHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	diffFrom := ""
	query := r.URL.Query()
	if df, exist := query["diff-from"]; exist {
		diffFrom = df[0]
	}
	name := vars["name"]
	go func() {
		api.status.start("upload")
		err := Upload(api.config, name, diffFrom)
		api.status.stop(err)
		if err != nil {
			log.Printf("Upload error: %+v\n", err)
			return
		}
	}()
	sendResponse(w, http.StatusOK, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
		BackupFrom string `json:"backup_from,omitempty"`
		Diff       bool   `json:"diff"`
	}{
		Status:     "acknowledged",
		Operation:  "upload",
		BackupName: name,
		BackupFrom: diffFrom,
		Diff:       diffFrom != "",
	})
}

// httpRestoreHandler - restore a backup from local storage
func (api *APIServer) httpRestoreHandler(w http.ResponseWriter, r *http.Request) {
	if locked := api.lock.TryAcquire(1); !locked {
		log.Println(ErrAPILocked)
		writeError(w, http.StatusLocked, "restore", ErrAPILocked)
		return
	}
	defer api.lock.Release(1)

	vars := mux.Vars(r)
	tablePattern := ""
	schemaOnly := false
	dataOnly := false
	dropTable := false
	partition := ""
	replicDb := ""

	query := r.URL.Query()
	if tp, exist := query["table"]; exist {
		tablePattern = tp[0]
	}
	if _, exist := query["schema"]; exist {
		schemaOnly = true
	}
	if _, exist := query["data"]; exist {
		dataOnly = true
	}
	if _, exist := query["drop"]; exist {
		dropTable = true
	}
	if _, exist := query["rm"]; exist {
		dropTable = true
	}
	api.status.start("restore")
	err := Restore(api.config, vars["name"], tablePattern, schemaOnly, dataOnly, dropTable,partition,replicDb)
	api.status.stop(err)
	if err != nil {
		log.Printf("Download error: %+v\n", err)
		writeError(w, http.StatusInternalServerError, "restore", err)
		return
	}
	sendResponse(w, http.StatusOK, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
	}{
		Status:     "success",
		Operation:  "restore",
		BackupName: vars["name"],
	})
}

// httpDownloadHandler - download a backup from remote to local storage
func (api *APIServer) httpDownloadHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]
	go func() {
		api.status.start("download")
		err := Download(api.config, name)
		api.status.stop(err)
		if err != nil {
			log.Printf("Download error: %+v\n", err)
			return
		}
	}()
	sendResponse(w, http.StatusOK, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
	}{
		Status:     "acknowledged",
		Operation:  "download",
		BackupName: name,
	})
}

// httpDeleteHandler - delete a backup from local or remote storage
func (api *APIServer) httpDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if locked := api.lock.TryAcquire(1); !locked {
		log.Println(ErrAPILocked)
		writeError(w, http.StatusLocked, "delete", ErrAPILocked)
		return
	}
	defer api.lock.Release(1)
	api.status.start("delete")
	var err error
	vars := mux.Vars(r)
	switch vars["where"] {
	case "local":
		err = RemoveBackupLocal(api.config, vars["name"])
	case "remote":
		err = RemoveBackupRemote(api.config, vars["name"])
	default:
		err = fmt.Errorf("Backup location must be 'local' or 'remote'")
	}
	api.status.stop(err)
	if err != nil {
		log.Printf("delete backup error: %+v\n", err)
		writeError(w, http.StatusInternalServerError, "delete", err)
		return
	}
	sendResponse(w, http.StatusOK, struct {
		Status     string `json:"status"`
		Operation  string `json:"operation"`
		BackupName string `json:"backup_name"`
		Location   string `json:"location"`
	}{
		Status:     "success",
		Operation:  "delete",
		BackupName: vars["name"],
		Location:   vars["where"],
	})
}

func (api *APIServer) httpBackupStatusHandler(w http.ResponseWriter, r *http.Request) {
	sendResponse(w, http.StatusOK, api.status.status())
}

func registerMetricsHandlers(r *mux.Router, enablemetrics bool, enablepprof bool) {
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		sendResponse(w, http.StatusOK, struct {
			Status string `json:"status"`
		}{
			Status: "OK",
		})
	})
	if enablemetrics {
		r.Handle("/metrics", promhttp.Handler())
	}
	if enablepprof {
		r.HandleFunc("/debug/pprof/", pprof.Index)
		r.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		r.HandleFunc("/debug/pprof/profile", pprof.Profile)
		r.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		r.HandleFunc("/debug/pprof/trace", pprof.Trace)
		r.Handle("/debug/pprof/block", pprof.Handler("block"))
		r.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		r.Handle("/debug/pprof/heap", pprof.Handler("heap"))
		r.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	}
}

type Metrics struct {
	LastBackupSuccess  prometheus.Gauge
	LastBackupStart    prometheus.Gauge
	LastBackupEnd      prometheus.Gauge
	LastBackupDuration prometheus.Gauge
	SuccessfulBackups  prometheus.Counter
	FailedBackups      prometheus.Counter
}

// setupMetrics - resister prometheus metrics
func setupMetrics() Metrics {
	m := Metrics{}
	m.LastBackupDuration = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "clickhouse_backup",
		Name:      "last_backup_duration",
		Help:      "Backup duration in nanoseconds.",
	})
	m.LastBackupSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "clickhouse_backup",
		Name:      "last_backup_success",
		Help:      "Last backup success boolean: 0=failed, 1=success, 2=unknown.",
	})
	m.LastBackupStart = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "clickhouse_backup",
		Name:      "last_backup_start",
		Help:      "Last backup start timestamp.",
	})
	m.LastBackupEnd = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "clickhouse_backup",
		Name:      "last_backup_end",
		Help:      "Last backup end timestamp.",
	})
	m.SuccessfulBackups = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "clickhouse_backup",
		Name:      "successful_backups",
		Help:      "Number of Successful Backups.",
	})
	m.FailedBackups = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "clickhouse_backup",
		Name:      "failed_backups",
		Help:      "Number of Failed Backups.",
	})
	prometheus.MustRegister(
		m.LastBackupDuration,
		m.LastBackupStart,
		m.LastBackupEnd,
		m.LastBackupSuccess,
		m.SuccessfulBackups,
		m.FailedBackups,
	)
	m.LastBackupSuccess.Set(2) // 0=failed, 1=success, 2=unknown
	return m
}
