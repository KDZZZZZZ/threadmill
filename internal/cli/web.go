package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/KDZZZZZZ/threadmill/internal/coordination"
	"github.com/KDZZZZZZ/threadmill/internal/event"
	"github.com/KDZZZZZZ/threadmill/internal/manager"
)

func runWeb(ctx context.Context, opts options, open func(context.Context, manager.Options) (*manager.Manager, error), out, errOut io.Writer) int {
	host, port, err := net.SplitHostPort(opts.listen)
	if err == nil {
		if host == "localhost" {
			host = "127.0.0.1"
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			err = fmt.Errorf("-listen must use a loopback IP address or localhost")
		}
	}
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	var publicOrigin *url.URL
	if opts.webOrigin != "" {
		publicOrigin, err = url.Parse(opts.webOrigin)
		if err != nil || publicOrigin.Hostname() == "" || publicOrigin.User != nil ||
			(publicOrigin.Scheme != "http" && publicOrigin.Scheme != "https") ||
			strings.Contains(publicOrigin.Host, "*") ||
			opts.webOrigin != publicOrigin.Scheme+"://"+publicOrigin.Host {
			fmt.Fprintln(errOut, "-web-origin must be an exact http(s) origin without credentials, path, query, fragment, or wildcard")
			return 1
		}
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	gateway, err := newWebGateway(ctx, opts.webUI, opts.configPath, open)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer gateway.close()
	gateway.publicOrigin = publicOrigin
	listener, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	defer listener.Close()
	_, gateway.port, _ = net.SplitHostPort(listener.Addr().String())
	server := &http.Server{
		Handler: gateway, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	// SSE handlers inherit cancellation; Close also unblocks pending network writes.
	stopClose := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stopClose()
	fmt.Fprintf(out, "Threadmill WebUI: http://%s\n", listener.Addr())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}

type webGateway struct {
	mu           sync.Mutex
	projects     map[string]*webProject
	open         func(manager.Options, func()) (*manager.Manager, error)
	uiPath       string
	configPath   string
	port         string
	publicOrigin *url.URL
	closed       bool
}

type webProject struct {
	opMu        sync.Mutex // Serializes user submissions and manager lifetime changes.
	mu          sync.Mutex // Callbacks never take opMu or the gateway registry lock.
	id          string
	root        string
	configPath  string
	manager     *manager.Manager
	model       string
	messages    []webMessage
	nextMessage uint64
	streamID    string
	receipts    map[string]webReceipt
	seq         uint64
	subscribers map[chan webEvent]struct{}
	agents      map[string]webAgent
	reasoning   map[string]webReasoning
	inflight    map[string]int
}

type webReceipt struct {
	Accepted    bool   `json:"accepted"`
	MessageID   string `json:"message_id"`
	QueueDepth  int    `json:"queue_depth"`
	contentHash [32]byte
}

type webEvent struct {
	ProjectID   string `json:"project_id"`
	Seq         string `json:"seq"`
	Data        any    `json:"data"`
	MessageID   string `json:"message_id,omitempty"`
	RoleAgentID string `json:"role_agent_id,omitempty"`
	name        string
}

type webAgent struct {
	ID             string    `json:"id"`
	TaskID         *string   `json:"task_id"`
	Role           string    `json:"role"`
	State          string    `json:"state"`
	Activity       string    `json:"activity"`
	UpdatedAt      time.Time `json:"updated_at"`
	CurrentTool    *string   `json:"current_tool"`
	CompletedSteps int       `json:"completed_steps"`
	TotalSteps     *int      `json:"total_steps"`
	CanMessage     bool      `json:"can_message"`
}

type webReasoning struct {
	Text      string        `json:"text"`
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration"`
	Running   bool          `json:"running"`
	Truncated bool          `json:"truncated"`
}

type webProjectView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Root         string `json:"root"`
	Model        string `json:"model"`
	ManagerID    string `json:"manager_id"`
	RuntimeState string `json:"runtime_state"`
	Busy         bool   `json:"busy"`
	Pending      int    `json:"pending"`
	TaskRunning  bool   `json:"task_running"`
	Error        string `json:"error,omitempty"`
}

type webMessage struct {
	ID        string    `json:"id"`
	Speaker   string    `json:"speaker"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func newWebGateway(ctx context.Context, uiPath, configPath string, open func(context.Context, manager.Options) (*manager.Manager, error)) (*webGateway, error) {
	var err error
	if uiPath != "" {
		uiPath, err = filepath.Abs(uiPath)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(uiPath)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("WebUI path must be a regular HTML file")
		}
	}
	if configPath != "" {
		configPath, err = filepath.Abs(configPath)
		if err != nil {
			return nil, err
		}
	}
	return &webGateway{projects: make(map[string]*webProject), uiPath: uiPath, configPath: configPath,
		open: func(opts manager.Options, reset func()) (*manager.Manager, error) {
			return open(event.WithDeltaResetSink(ctx, reset), opts)
		}}, nil
}

func (g *webGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !g.allowedRequest(r) {
		webError(w, http.StatusForbidden, "request must come from an allowed WebUI origin")
		return
	}
	if r.Method == http.MethodPost {
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			webError(w, http.StatusUnsupportedMediaType, "application/json is required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		defer r.Body.Close()
	}
	if r.URL.Path == "/" && r.Method == http.MethodGet && g.uiPath != "" {
		http.ServeFile(w, r, g.uiPath)
		return
	}
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		webJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "threadmill-webui"})
		return
	}
	if r.URL.Path == "/api/v1/projects" {
		switch r.Method {
		case http.MethodPost:
			g.openProject(w, r)
		case http.MethodGet:
			g.mu.Lock()
			projects := make([]*webProject, 0, len(g.projects))
			for _, p := range g.projects {
				projects = append(projects, p)
			}
			g.mu.Unlock()
			items := make([]webProjectView, 0, len(projects))
			for _, p := range projects {
				items = append(items, p.view(r.Context()))
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Root < items[j].Root })
			webJSON(w, http.StatusOK, map[string]any{"items": items})
		default:
			webError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/projects/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/api/v1/projects/") || len(parts) > 2 {
		webError(w, http.StatusNotFound, "not found")
		return
	}
	g.mu.Lock()
	p := g.projects[parts[0]]
	g.mu.Unlock()
	if p == nil {
		webError(w, http.StatusNotFound, "project not found")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		webJSON(w, http.StatusOK, p.view(r.Context()))
		return
	}
	if len(parts) == 2 && parts[1] == "messages" && r.Method == http.MethodGet {
		p.mu.Lock()
		items := append([]webMessage{}, p.messages...)
		p.mu.Unlock()
		webJSON(w, http.StatusOK, map[string]any{"items": items, "next_before": nil})
		return
	}
	if len(parts) == 2 {
		switch {
		case parts[1] == "messages" && r.Method == http.MethodPost:
			p.send(w, r)
			return
		case parts[1] == "events" && r.Method == http.MethodGet:
			p.events(w, r)
			return
		case (parts[1] == "cancel" || parts[1] == "close") && r.Method == http.MethodPost:
			if !webDecode(w, r, &struct{}{}) {
				return
			}
			p.opMu.Lock()
			defer p.opMu.Unlock()
			if parts[1] == "close" {
				p.closeManager()
				webJSON(w, http.StatusOK, p.viewLocked(r.Context()))
				return
			}
			canceled := p.manager != nil && p.manager.Cancel()
			webJSON(w, http.StatusOK, map[string]bool{"canceled": canceled})
			return
		case parts[1] == "agents" && r.Method == http.MethodGet:
			p.opMu.Lock()
			p.refreshAgents()
			p.mu.Lock()
			items := p.agentViewsLocked()
			p.mu.Unlock()
			p.opMu.Unlock()
			webJSON(w, http.StatusOK, map[string]any{"items": items})
			return
		case (parts[1] == "graph" || parts[1] == "metrics") && r.Method == http.MethodGet:
			p.opMu.Lock()
			defer p.opMu.Unlock()
			if p.manager == nil {
				webError(w, http.StatusConflict, "project is closed")
				return
			}
			if parts[1] == "graph" {
				webJSON(w, http.StatusOK, webGraphSnapshot(p.manager.Snapshot()))
			} else {
				webJSON(w, http.StatusOK, p.manager.Metrics())
			}
			return
		}
	}
	webError(w, http.StatusNotFound, "not found")
}

func (g *webGateway) openProject(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Root       string `json:"root"`
		ConfigPath string `json:"config_path"`
	}
	if !webDecode(w, r, &request) {
		return
	}
	if request.Root == "" {
		webError(w, http.StatusBadRequest, "root is required")
		return
	}
	root, err := filepath.Abs(request.Root)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		webError(w, http.StatusBadRequest, err.Error())
		return
	}
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		webError(w, http.StatusBadRequest, "root must be an existing directory")
		return
	}
	configPath := request.ConfigPath
	if configPath == "" {
		configPath = g.configPath
	}
	if configPath != "" && !filepath.IsAbs(configPath) {
		configPath = filepath.Join(root, configPath)
	}
	id := fmt.Sprintf("project-%x", sha256.Sum256([]byte(root)))
	// ponytail: serialize project opens; separate in-flight entries if many slow opens need concurrency.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		webError(w, http.StatusServiceUnavailable, "WebUI is stopping")
		return
	}
	p := g.projects[id]
	if p != nil {
		p.opMu.Lock()
		defer p.opMu.Unlock()
		if p.manager != nil && webManagerFailure(r.Context(), p.manager) != nil {
			p.closeManager()
		}
		if p.manager != nil {
			if request.ConfigPath != "" && p.configPath != configPath {
				webError(w, http.StatusConflict, "project is already open with a different configuration")
				return
			}
			webJSON(w, http.StatusOK, p.viewLocked(r.Context()))
			return
		}
	} else {
		p = &webProject{id: id, root: root, receipts: make(map[string]webReceipt), subscribers: make(map[chan webEvent]struct{}), agents: make(map[string]webAgent), reasoning: make(map[string]webReasoning)}
		p.opMu.Lock()
		defer p.opMu.Unlock()
	}
	p.configPath = configPath
	p.mu.Lock()
	p.agents["manager"] = webAgent{ID: "manager", Role: "manager", State: "idle", CanMessage: true, UpdatedAt: time.Now()}
	p.mu.Unlock()
	mgr, err := g.open(manager.Options{Root: root, ConfigPath: configPath, Output: p.output, OnEvent: p.onEvent}, p.resetStream)
	if err != nil {
		webError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	p.mu.Lock()
	p.manager = mgr
	p.model = mgr.ModelName()
	p.mu.Unlock()
	status := http.StatusOK
	if g.projects[id] == nil {
		status = http.StatusCreated
		g.projects[id] = p
	}
	webJSON(w, status, p.viewLocked(r.Context()))
}

func (p *webProject) view(ctx context.Context) webProjectView {
	p.opMu.Lock()
	defer p.opMu.Unlock()
	return p.viewLocked(ctx)
}

func (p *webProject) viewLocked(ctx context.Context) webProjectView {
	mgr := p.manager
	view := webProjectView{ID: p.id, Name: filepath.Base(p.root), Root: p.root, Model: p.model, ManagerID: "manager", RuntimeState: "closed"}
	if mgr != nil {
		metrics := mgr.Metrics()
		view.Model, view.RuntimeState, view.Busy = mgr.ModelName(), "open", mgr.Busy()
		view.Pending, view.TaskRunning = metrics.Pending, metrics.Tasks.Running > 0
		if err := webManagerFailure(ctx, mgr); err != nil {
			view.RuntimeState = "error"
			view.Error = err.Error()
			view.Busy = view.TaskRunning || metrics.Events.Model.Active > 0 ||
				metrics.Events.Tool.Active > 0 || metrics.Events.Memory.Active > 0
		}
	}
	return view
}

func (p *webProject) output(content string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	kind := "message"
	if strings.HasPrefix(content, "[任务报告]") {
		kind = "task_report"
	}
	message := webMessage{ID: p.streamID, Speaker: "manager", Kind: kind, Content: content, Status: "complete", CreatedAt: time.Now()}
	if kind == "task_report" || message.ID == "" {
		message.ID = p.messageIDLocked()
	}
	found := false
	for i := range p.messages {
		if p.messages[i].ID == message.ID {
			message.CreatedAt = p.messages[i].CreatedAt
			p.messages[i] = message
			found = true
			break
		}
	}
	if !found {
		p.messages = append(p.messages, message)
	}
	if kind != "task_report" {
		p.streamID = ""
	}
	p.publishLocked("output", message, "")
}

func (p *webProject) messageIDLocked() string {
	p.nextMessage++
	return fmt.Sprintf("message-%d", p.nextMessage)
}

func (p *webProject) send(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Content         string `json:"content"`
		ClientMessageID string `json:"client_message_id"`
	}
	if !webDecode(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Content) == "" || request.ClientMessageID == "" || len(request.ClientMessageID) > 256 {
		webError(w, http.StatusBadRequest, "content and client_message_id are required")
		return
	}
	p.opMu.Lock()
	defer p.opMu.Unlock()
	if p.manager == nil {
		webError(w, http.StatusConflict, "project is closed")
		return
	}
	p.mu.Lock()
	if receipt, ok := p.receipts[request.ClientMessageID]; ok {
		p.mu.Unlock()
		if receipt.contentHash != sha256.Sum256([]byte(request.Content)) {
			webError(w, http.StatusConflict, "client_message_id was used with different content")
			return
		}
		webJSON(w, http.StatusAccepted, receipt)
		return
	}
	if len(p.receipts) >= 10000 {
		p.mu.Unlock()
		webError(w, http.StatusTooManyRequests, "project reached the process-local receipt limit")
		return
	}
	p.mu.Unlock()
	if err := webManagerFailure(r.Context(), p.manager); err != nil {
		webError(w, http.StatusConflict, "manager stopped: "+err.Error()+"; reopen the project")
		return
	}
	p.mu.Lock()
	message := webMessage{ID: p.messageIDLocked(), Speaker: "user", Kind: "message", Content: request.Content, Status: "queued", CreatedAt: time.Now()}
	p.messages = append(p.messages, message)
	p.publishLocked("output", message, "")
	p.mu.Unlock()
	p.manager.Send(request.Content)
	receipt := webReceipt{Accepted: true, MessageID: message.ID, QueueDepth: p.manager.Metrics().Pending, contentHash: sha256.Sum256([]byte(request.Content))}
	p.mu.Lock()
	p.receipts[request.ClientMessageID] = receipt
	p.mu.Unlock()
	webJSON(w, http.StatusAccepted, receipt)
}

func (p *webProject) onEvent(_ context.Context, ev event.RuntimeEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if ev.AgentID != "manager" {
		ev.Delta = ""
	}
	agentID, taskID, role := webAgentIdentity(ev.AgentID)
	a, known := p.agents[agentID]
	if !known && role != "" {
		a = webAgent{ID: agentID, Role: role, State: "unknown"}
		if taskID != "" {
			a.TaskID = &taskID
		}
		known = true
	}
	if known {
		if p.inflight == nil {
			p.inflight = make(map[string]int)
		}
		if ev.Phase == event.PhaseStart {
			p.inflight[agentID]++
		}
		if ev.Phase == event.PhaseEnd && p.inflight[agentID] > 0 {
			p.inflight[agentID]--
		}
		a.UpdatedAt = ev.Time
		switch ev.Phase {
		case event.PhaseStart:
			a.State = "running"
			a.Activity = string(ev.Kind)
			if ev.Kind == event.KindTool {
				name := ev.Name
				a.CurrentTool = &name
			}
		case event.PhaseEnd:
			a.State = "idle"
			a.Activity = ""
			if ev.Err != "" || ev.IsError {
				a.State = "failed"
			}
			if ev.Kind == event.KindTool {
				a.CurrentTool = nil
				a.CompletedSteps++
			}
		case event.PhaseRetry:
			a.State = "running"
			a.Activity = string(ev.Kind)
		case event.PhaseDelta:
			a.State = "running"
			a.Activity = string(ev.Kind)
		}
		if p.inflight[agentID] > 0 && ev.Phase == event.PhaseEnd {
			a.State = "running"
		}
		if a.CurrentTool != nil && *a.CurrentTool == "coordination_requestHelp" {
			a.State = "waiting"
		}
		p.agents[agentID] = a
		if ev.Kind == event.KindModel {
			trace := p.reasoning[agentID]
			if ev.Phase == event.PhaseStart {
				trace = webReasoning{StartedAt: ev.Time, Running: true}
			}
			if trace.StartedAt.IsZero() {
				trace.StartedAt = ev.Time
				trace.Running = true
			}
			trace.Text += ev.ReasoningDelta
			if len(trace.Text) > 65536 {
				trace.Text = trace.Text[len(trace.Text)-65536:]
				for len(trace.Text) > 0 && !utf8.RuneStart(trace.Text[0]) {
					trace.Text = trace.Text[1:]
				}
				trace.Truncated = true
			}
			if ev.Phase == event.PhaseEnd {
				trace.Running = false
				trace.Duration = ev.Duration
			}
			p.reasoning[agentID] = trace
		}
	}
	if ev.Kind == event.KindTask && ev.Phase == event.PhaseEnd {
		for id, a := range p.agents {
			if a.TaskID != nil && *a.TaskID == agentID {
				a.State = ev.Name
				a.Activity = ""
				a.CurrentTool = nil
				a.UpdatedAt = ev.Time
				p.agents[id] = a
			}
		}
	}
	messageID := ""
	if ev.AgentID == "manager" && ev.Kind == event.KindModel && ev.Delta != "" {
		if p.streamID == "" {
			p.streamID = p.messageIDLocked()
			p.messages = append(p.messages, webMessage{ID: p.streamID, Speaker: "manager", Kind: "message", Status: "streaming", CreatedAt: ev.Time})
		}
		messageID = p.streamID
		for i := range p.messages {
			if p.messages[i].ID == messageID {
				p.messages[i].Content += ev.Delta
				break
			}
		}
	}
	if ev.Kind == event.KindModel && ev.AgentID == "manager" && ev.Phase == event.PhaseEnd && ev.Err != "" {
		for i := range p.messages {
			if p.messages[i].ID == p.streamID {
				p.messages[i].Status = "error"
				p.publishLocked("output", p.messages[i], "")
			}
		}
		p.streamID = ""
	}
	p.publishLocked("runtime_event", ev, messageID)
}

func (p *webProject) publishLocked(name string, data any, messageID string) {
	if len(p.messages) > 256 {
		p.messages = append([]webMessage(nil), p.messages[len(p.messages)-256:]...)
	}
	p.seq++
	ev := webEvent{ProjectID: p.id, Seq: strconv.FormatUint(p.seq, 10), Data: data, MessageID: messageID, name: name}
	if runtime, ok := data.(event.RuntimeEvent); ok {
		ev.RoleAgentID, _, _ = webAgentIdentity(runtime.AgentID)
	}
	for channel := range p.subscribers {
		select {
		case channel <- ev:
		default:
			delete(p.subscribers, channel)
			close(channel)
		}
	}
}

func (p *webProject) agentViewsLocked() []webAgent {
	items := make([]webAgent, 0, len(p.agents))
	for _, a := range p.agents {
		items = append(items, a)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// Caller owns opMu. Seed roles without inventing progress for events we did not observe.
func (p *webProject) refreshAgents() {
	if p.manager == nil {
		return
	}
	graph := p.manager.Snapshot()
	p.mu.Lock()
	defer p.mu.Unlock()
	completed := make(map[string]bool, len(graph.Outputs))
	for _, output := range graph.Outputs {
		completed[output.Node.ID] = true
	}
	for _, task := range graph.Tasks {
		for _, node := range task.Sequence() {
			a, ok := p.agents[node.ID]
			if !ok {
				taskID := task.ID
				a = webAgent{ID: node.ID, TaskID: &taskID, Role: node.Role, State: "unknown", UpdatedAt: time.Now()}
			}
			if a.State == "unknown" {
				a.State = "waiting"
			}
			if completed[node.ID] && p.inflight[node.ID] == 0 {
				a.State = "done"
			}
			if task.Outcome != "active" && p.inflight[node.ID] == 0 {
				a.State = task.Outcome
				a.Activity = ""
				a.CurrentTool = nil
			}
			p.agents[node.ID] = a
		}
	}
}

func (p *webProject) events(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	p.opMu.Lock()
	if p.manager == nil {
		p.opMu.Unlock()
		webError(w, http.StatusConflict, "project is closed")
		return
	}
	project, graph, metrics := p.viewLocked(r.Context()), webGraphSnapshot(p.manager.Snapshot()), p.manager.Metrics()
	p.refreshAgents()
	channel := make(chan webEvent, 64)
	p.mu.Lock()
	traces := make(map[string]webReasoning, len(p.reasoning))
	for id, trace := range p.reasoning {
		traces[id] = trace
	}
	snapshot := webEvent{ProjectID: p.id, Seq: strconv.FormatUint(p.seq, 10), name: "snapshot", Data: map[string]any{
		"project": project, "graph": graph, "metrics": metrics, "agents": p.agentViewsLocked(), "reasoning": traces,
		"messages": map[string]any{"items": append([]webMessage{}, p.messages...), "next_before": nil},
	}}
	p.subscribers[channel] = struct{}{}
	p.mu.Unlock()
	p.opMu.Unlock()
	defer func() { p.mu.Lock(); delete(p.subscribers, channel); p.mu.Unlock() }()
	write := func(ev webEvent) error {
		body, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		defer controller.SetWriteDeadline(time.Time{})
		if _, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.Seq, ev.name, body); err != nil {
			return err
		}
		return controller.Flush()
	}
	if write(snapshot) != nil {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-channel:
			if !ok || write(ev) != nil {
				return
			}
		case <-ticker.C:
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
			_ = controller.SetWriteDeadline(time.Time{})
		}
	}
}

// WaitIdle reports a terminal manager error before context cancellation. Cancel
// this observation immediately so checking health never waits for active work.
func webManagerFailure(ctx context.Context, mgr *manager.Manager) error {
	probe, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cancel()
	err := mgr.WaitIdle(probe)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func webDecode(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		webError(w, status, "invalid JSON: "+err.Error())
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		webError(w, status, "request must contain one JSON object")
		return false
	}
	return true
}

func (g *webGateway) allowedRequest(r *http.Request) bool {
	u, err := url.Parse("http://" + r.Host)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	expectedOrigin := scheme + "://" + r.Host
	if g.publicOrigin != nil && r.Host == g.publicOrigin.Host {
		// The loopback listener sits behind an explicitly configured proxy.
		// Do not infer trusted origins from caller-controlled forwarding headers.
		expectedOrigin = g.publicOrigin.String()
	} else {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return false
		}
		if g.port != "" && u.Port() != g.port {
			return false
		}
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != expectedOrigin {
		return false
	}
	return r.Header.Get("Sec-Fetch-Site") != "cross-site"
}

func (g *webGateway) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	for _, p := range g.projects {
		p.opMu.Lock()
		p.closeManager()
		p.opMu.Unlock()
	}
}

// Caller owns opMu; callbacks may still arrive until Close has joined the manager.
func (p *webProject) closeManager() {
	if p.manager == nil {
		return
	}
	p.manager.Close()
	p.mu.Lock()
	p.manager = nil
	p.streamID = ""
	for id, a := range p.agents {
		a.State = "idle"
		a.Activity = ""
		a.CurrentTool = nil
		p.agents[id] = a
	}
	for id, trace := range p.reasoning {
		trace.Running = false
		p.reasoning[id] = trace
	}
	for channel := range p.subscribers {
		delete(p.subscribers, channel)
		close(channel)
	}
	p.mu.Unlock()
}

func webJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func webError(w http.ResponseWriter, status int, message string) {
	webJSON(w, status, map[string]any{"error": map[string]string{"message": message}})
}

// Checkpoints retain their exact event ID on the wire; aggregate their status under the role node.
func webAgentIdentity(id string) (string, string, string) {
	if id == "manager" {
		return id, "", "manager"
	}
	parts := strings.Split(id, ":")
	for i := 1; i < len(parts); i++ {
		if parts[i] == "planner" || parts[i] == "executor" || parts[i] == "verifier" {
			return strings.Join(parts[:i+1], ":"), parts[0], parts[i]
		}
	}
	if strings.Contains(id, "organizer") {
		return id, "", "organizer"
	}
	return id, "", ""
}

// The UI needs graph metadata, not private immutable files or memory storage references.
func webGraphSnapshot(s coordination.Snapshot) any {
	return struct {
		ProjectTaskID string              `json:"project_task_id,omitempty"`
		Revision      int64               `json:"revision"`
		Executing     bool                `json:"executing"`
		Tasks         []coordination.Task `json:"tasks"`
		Nodes         []coordination.Node `json:"nodes"`
		Edges         []coordination.Edge `json:"edges"`
	}{s.ProjectTaskID, s.Revision, s.Executing, s.Tasks, s.Nodes, s.Edges}
}

// Only the incomplete model response is retractable; complete outputs and user submissions survive retries.
func (p *webProject) resetStream() {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := p.streamID
	for i, m := range p.messages {
		if m.ID == id && m.Status == "streaming" {
			p.messages = append(p.messages[:i], p.messages[i+1:]...)
			break
		}
	}
	p.streamID = ""
	p.reasoning["manager"] = webReasoning{StartedAt: time.Now(), Running: true}
	p.publishLocked("stream_reset", map[string]string{"message_id": id, "agent_id": "manager"}, "")
}
