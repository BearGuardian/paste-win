package main

import (
	"context"
	"embed"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"pastewin/internal/blobs"
	"pastewin/internal/clipwatch"
	"pastewin/internal/exclusion"
	"pastewin/internal/hotkey"
	"pastewin/internal/model"
	"pastewin/internal/paster"
	"pastewin/internal/store"
	"pastewin/internal/tray"
	"pastewin/internal/winapi"
)

// logMaxBytes 是日志文件的大小上限,超过就轮转掉上一份。
const logMaxBytes = 2 << 20

// tolerantWriter 吞掉底层写入错误,只报告写入长度。
//
// 不能直接用 io.MultiWriter:它在**第一个** writer 上报错就立即返回,
// 后面的 writer 一个字节都拿不到。托盘应用常以无控制台的方式启动
// (开机自启、快捷方式、`start`),此时 os.Stderr 是无效句柄,
// 于是日志文件永远是空的 —— 排查时表现为"根本没有日志可看"。
type tolerantWriter struct{ w io.Writer }

func (t tolerantWriter) Write(p []byte) (int, error) {
	_, _ = t.w.Write(p)
	return len(p), nil
}

// setupLogging 让日志同时落到数据目录下的 pastewin.log。
//
// 托盘应用没有控制台:脱离终端启动后 stdout 无处可去,
// 出问题时只能靠猜。回贴这条链路尤其依赖时序,没有日志基本无法定位。
func setupLogging(dir string) func() {
	path := filepath.Join(dir, "pastewin.log")

	// 只保留一代历史:这是排障日志,不是审计日志
	if info, err := os.Stat(path); err == nil && info.Size() > logMaxBytes {
		_ = os.Rename(path, path+".1")
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("无法写入日志文件,仅输出到标准错误: %v", err)
		return func() {}
	}
	log.SetOutput(io.MultiWriter(tolerantWriter{f}, tolerantWriter{os.Stderr}))
	log.Printf("---- 日志开始,写入 %s ----", path)
	return func() { f.Close() }
}


//go:embed all:frontend/dist
var assets embed.FS

// mainWindowTitle 必须与 options.App.Title 一致 ——
// 唤起面板时靠它 FindWindow 找回自己的窗口句柄。
const mainWindowTitle = "PasteWin"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[PasteWin] ")

	// --quit:通知已在运行的实例退出,自己随即结束。
	// 供卸载脚本与脚本化场景使用 —— 比 taskkill 好,走的是正常退出路径,
	// 会执行清理、注销热键、移除托盘图标。
	if len(os.Args) > 1 && os.Args[1] == "--quit" {
		instance, err := winapi.AcquireSingleInstance("PasteWin")
		if err != nil {
			log.Fatalf("无法访问单实例状态: %v", err)
		}
		defer instance.Close()

		if !instance.AlreadyRunning() {
			log.Println("PasteWin 未在运行")
			return
		}
		if err := instance.SignalQuit(); err != nil {
			log.Fatalf("发送退出信号失败: %v", err)
		}
		log.Println("已请求退出")
		return
	}

	if err := run(); err != nil {
		log.Fatalf("启动失败: %v", err)
	}
}

func run() error {
	// 单实例守卫。必须在打开数据库之前:多个实例同时持有 SQLite 连接会互相干扰,
	// 而且每个实例都会往托盘塞一个图标、各自注册一遍全局热键。
	instance, err := winapi.AcquireSingleInstance("PasteWin")
	if err != nil {
		return err
	}
	defer instance.Close()

	if instance.AlreadyRunning() {
		// 再次启动(双击快捷方式、开机自启撞上手动启动)时,
		// 不报错,而是把已在运行的实例的面板叫出来。
		if err := instance.NotifyExisting(); err != nil {
			log.Printf("唤醒已有实例失败: %v", err)
		}
		log.Println("PasteWin 已在运行,已唤起其面板")
		return nil
	}

	// ---- 数据层 ----
	dir, err := store.DefaultDir()
	if err != nil {
		return err
	}

	closeLog := setupLogging(dir)
	defer closeLog()

	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer st.Close()

	bl, err := blobs.New(dir)
	if err != nil {
		return err
	}

	settings, err := st.LoadSettings()
	if err != nil {
		return err
	}
	excluded := exclusion.New(settings.ExcludedApps)

	// ---- 服务层 ----
	// 消息窗口是剪贴板通知与全局热键的唯一事件源。它的回调需要引用
	// 后面才创建的 App,因此用闭包捕获一个稍后赋值的变量。
	var appPtr *App
	var hotkeys *hotkey.Manager
	var trayIcon *tray.Tray

	watcher := clipwatch.New(
		func(c model.ClipContent) { appPtr.OnCaptured(c) },
		func(reason string) { log.Printf("跳过: %s", reason) },
	)
	watcher.SetExcluder(excluded.ShouldSkip)
	watcher.SetPaused(settings.PauseCapture)

	mw, err := winapi.StartMessageWindow(func(hwnd uintptr, msg uint32, wParam, lParam uintptr) (uintptr, bool) {
		// 托盘回调消息先交给托盘处理
		if trayIcon != nil && trayIcon.HandleMessage(msg, lParam) {
			return 0, true
		}
		switch msg {
		case winapi.WM_CLIPBOARDUPDATE:
			watcher.OnClipboardUpdate()
			return 0, true
		case winapi.WM_HOTKEY:
			log.Printf("收到热键 id=%d", wParam)
			if hotkeys != nil {
				hotkeys.Dispatch(int(wParam))
			}
			return 0, true
		}
		return 0, false
	})
	if err != nil {
		return err
	}

	if err := winapi.AddClipboardFormatListener(mw.HWND()); err != nil {
		return err
	}

	hotkeys = hotkey.New(mw)

	pstr := paster.New(bl)
	pstr.SetIgnoreNextWrite(watcher.IgnoreNextWrite)

	app := NewApp(st, bl, watcher, pstr, hotkeys, mw, excluded, settings)
	appPtr = app

	// 第二个实例启动时,把面板叫出来
	notifyStop := make(chan struct{})
	defer close(notifyStop)
	instance.OnNotify(notifyStop, func() {
		log.Println("收到另一实例的唤醒请求")
		app.ShowPanel()
	})
	instance.OnQuit(notifyStop, func() {
		log.Println("收到退出请求")
		app.Quit()
	})

	// 托盘。图标在运行时生成到数据目录,不依赖内嵌资源。
	trayIcon, err = tray.New(mw, dir, "PasteWin — 剪贴板历史", app.HandleTrayCommand)
	if err != nil {
		// 托盘失败不阻塞主功能:面板靠热键仍可正常使用
		log.Printf("创建托盘图标失败(应用仍可使用热键): %v", err)
		trayIcon = nil
	} else {
		app.SetTray(trayIcon)
		defer trayIcon.Close()
	}

	// ---- 应用 ----
	handler := &assetHandler{store: st, blobs: bl}

	err = wails.Run(&options.App{
		Title:       mainWindowTitle,
		Width:       1200,
		Height:      settings.PanelHeight,
		MinWidth:    600,
		MinHeight:   160,
		Frameless:   true, // 无边框,圆角与背景由 CSS 自绘
		AlwaysOnTop: true,
		StartHidden: true, // 常驻后台,由热键唤起
		AssetServer: &assetserver.Options{
			Assets:  assets,
			Handler: handler,
		},
		OnStartup:  app.Startup,
		OnShutdown: app.Shutdown,
		OnBeforeClose: func(ctx context.Context) bool {
			// 点窗口关闭按钮时只收起面板,程序继续常驻托盘。
			// 但真正的退出必须放行 —— Wails 的 Quit 会先调用本钩子,
			// 返回 true 就直接放弃退出。
			if app.IsQuitting() {
				return false
			}
			app.HidePanel()
			return true
		},
		Bind: []any{app},
		Windows: &windows.Options{
			WebviewIsTransparent: true, // 让 CSS 的半透明背景生效
			WindowIsTranslucent:  true, // 亚克力模糊
			DisableWindowIcon:    false,
		},
	})

	log.Printf("wails.Run 已返回 (err=%v)", err)
	return err
}

// assetHandler 直接以 HTTP 提供图片文件。
//
// 缩略图若走 Wails 的绑定方法返回 base64,每个条目要额外编码几十 KB 并
// 穿过一次 IPC;走 HTTP 则浏览器原生加载并缓存,列表滚动才不卡。
type assetHandler struct {
	store *store.Store
	blobs *blobs.Store
}

func (h *assetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/thumb/"):
		h.serve(w, r, strings.TrimPrefix(r.URL.Path, "/thumb/"))
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		h.serve(w, r, strings.TrimPrefix(r.URL.Path, "/blob/"))
	default:
		http.NotFound(w, r)
	}
}

func (h *assetHandler) serve(w http.ResponseWriter, r *http.Request, id string) {
	it, err := h.store.Get(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rel := it.ThumbPath
	if strings.HasPrefix(r.URL.Path, "/blob/") || rel == "" {
		rel = it.BlobPath
	}
	if rel == "" {
		http.NotFound(w, r)
		return
	}

	abs, err := h.blobs.Abs(rel)
	if err != nil {
		http.Error(w, "路径非法", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, abs)
}
