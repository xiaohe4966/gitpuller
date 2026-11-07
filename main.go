package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const appVersion = "1.0.0"

//go:embed ui.html
var uiHTML string

func logf(format string, args ...any) {
	log.Printf(format, args...)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:7788", "HTTP 监听地址")
	cfgPath := flag.String("config", "", "配置文件路径（默认 ~/.gitpuller/config.json）")
	open := flag.Bool("open", true, "启动后自动打开浏览器")
	once := flag.Bool("once", false, "只拉取一轮后退出（适合 cron / launchd）")
	quiet := flag.Bool("quiet", false, "不打印启动信息")
	flag.Parse()

	if *cfgPath == "" {
		*cfgPath = defaultConfigPath()
	}

	svc, err := NewService(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	if *once {
		os.Exit(svc.RunOnce())
	}

	svc.StartScheduler()

	mux := newMux(svc)

	// 端口被占用时自动往后找
	ln, realAddr, err := listenWithFallback(*addr)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	url := "http://" + realAddr
	if !*quiet {
		logf("GitPuller %s 已启动 → %s", appVersion, url)
		logf("配置文件: %s | 共 %d 个目录 | 定时任务: %v", *cfgPath, len(svc.Snapshot().Projects), onOff(svc.Snapshot().RunnerOn))
	}
	if *open {
		go func() {
			time.Sleep(400 * time.Millisecond)
			openBrowser(url)
		}()
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("服务异常退出: %v", err)
	}
}

func onOff(b bool) string {
	if b {
		return "已开启"
	}
	return "已暂停"
}

func listenWithFallback(addr string) (net.Listener, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, "", err
	}
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		return nil, "", err
	}
	var lastErr error
	for i := 0; i < 12; i++ {
		candidate := net.JoinHostPort(host, fmt.Sprint(p+i))
		ln, err := net.Listen("tcp", candidate)
		if err == nil {
			return ln, candidate, nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

func newMux(svc *Service) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(uiHTML))
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.Snapshot())
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "不支持流式响应", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		ch := svc.Subscribe()
		defer svc.Unsubscribe(ch)

		// 先推一份完整状态，页面刷新后不用等
		if b, err := json.Marshal(Event{Type: "state", State: ptr(svc.Snapshot())}); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}

		heartbeat := time.NewTicker(20 * time.Second)
		defer heartbeat.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case b := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			case <-heartbeat.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	})

	mux.HandleFunc("POST /api/projects", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path     string `json:"path"`
			Interval int    `json:"interval"`
			Strategy string `json:"strategy"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		p, err := svc.Add(req.Path, req.Interval, req.Strategy)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		svc.save()
		svc.Broadcast()
		if svc.Snapshot().RunnerOn {
			svc.Pull(p.ID, false)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "project": p, "state": svc.Snapshot()})
	})

	mux.HandleFunc("PATCH /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req patchReq
		if err := decode(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := svc.Patch(r.PathValue("id"), req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": svc.Snapshot()})
	})

	mux.HandleFunc("DELETE /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !svc.Remove(r.PathValue("id")) {
			writeErr(w, http.StatusBadRequest, "项目不存在或正在拉取中")
			return
		}
		svc.save()
		svc.Broadcast()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": svc.Snapshot()})
	})

	mux.HandleFunc("POST /api/projects/{id}/pull", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		svc.mu.RLock()
		exists := svc.find(id) != nil
		svc.mu.RUnlock()
		if !exists {
			writeErr(w, http.StatusNotFound, "项目不存在")
			return
		}
		svc.Pull(id, true)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /api/projects/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"logs": svc.Logs(r.PathValue("id"))})
	})

	mux.HandleFunc("POST /api/runner", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On bool `json:"on"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		svc.SetRunner(req.On)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": svc.Snapshot()})
	})

	mux.HandleFunc("POST /api/pick-folder", func(w http.ResponseWriter, r *http.Request) {
		p, err := pickFolder()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": p})
	})

	return mux
}

func ptr[T any](v T) *T { return &v }

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求参数错误: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// pickFolder 调系统原生选择框（不支持时返回错误，前端降级为手输路径）
func pickFolder() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("osascript", "-e",
			`POSIX path of (choose folder with prompt "选择 Git 仓库目录")`).Output()
		if err != nil {
			return "", errors.New("未选择目录或系统未授予权限，可直接粘贴路径")
		}
		return strings.TrimSpace(string(out)), nil
	case "windows":
		script := `Add-Type -AssemblyName System.Windows.Forms;` +
			`$d = New-Object System.Windows.Forms.FolderBrowserDialog;` +
			`$d.Description = '选择 Git 仓库目录';` +
			`if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $d.SelectedPath }`
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
		if err != nil {
			return "", errors.New("未选择目录，可直接粘贴路径")
		}
		return strings.TrimSpace(string(out)), nil
	default:
		if _, err := exec.LookPath("zenity"); err == nil {
			out, err := exec.Command("zenity", "--file-selection", "--directory", "--title=选择 Git 仓库目录").Output()
			if err != nil {
				return "", errors.New("未选择目录，可直接粘贴路径")
			}
			return strings.TrimSpace(string(out)), nil
		}
		if _, err := exec.LookPath("kdialog"); err == nil {
			out, err := exec.Command("kdialog", "--getexistingdirectory", ".", "--title", "选择 Git 仓库目录").Output()
			if err != nil {
				return "", errors.New("未选择目录，可直接粘贴路径")
			}
			return strings.TrimSpace(string(out)), nil
		}
		return "", errors.New("当前系统没有可用的文件选择框，请直接粘贴目录路径")
	}
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", url).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		if _, err := exec.LookPath("xdg-open"); err == nil {
			_ = exec.Command("xdg-open", url).Start()
		}
	}
}
