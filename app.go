package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"pastewin/internal/autostart"
	"pastewin/internal/blobs"
	"pastewin/internal/clipwatch"
	"pastewin/internal/exclusion"
	"pastewin/internal/hotkey"
	"pastewin/internal/model"
	"pastewin/internal/ocr"
	"pastewin/internal/paster"
	"pastewin/internal/store"
	"pastewin/internal/tray"
	"pastewin/internal/winapi"
)

// App 是暴露给前端的绑定层。
//
// 前端通过 window.go.main.App.Xxx() 调用其导出方法;Go 侧通过
// runtime.EventsEmit 推送 clip:new / items:changed / error 事件。
type App struct {
	ctx context.Context

	store    *store.Store
	blobs    *blobs.Store
	watcher  *clipwatch.Watcher
	paster   *paster.Paster
	hotkeys  *hotkey.Manager
	msgwin   *winapi.MessageWindow
	excluded *exclusion.List

	mu          sync.Mutex
	settings    model.Settings
	hotkeyID    int
	prevTarget  winapi.FocusTarget // 唤起面板前的焦点目标(窗口+控件),回贴时还原
	panelShown  bool
	resizing    bool        // 正在拖拽改高,期间不接受新的拖拽
	panelBounds winapi.RECT // 最近一次由 ShowPanel 设下去的物理矩形,用于判断用户是否改过大小
	tray        *tray.Tray
	quitting    atomic.Bool // 见 Quit():用于让 OnBeforeClose 放行真正的退出
	cleanupOnce sync.Once

	// 后台 OCR。引擎为 nil 表示本机不可用,整条链路关闭。
	ocrEngine *ocr.Engine
	ocrQueue  chan store.PendingOCR
	ocrStop   chan struct{}
	ocrWG     sync.WaitGroup
}

// NewApp 构造绑定层。依赖由 main 组装后注入,便于测试时替换。
// ocrEngine 可为 nil,表示本机没有可用的 OCR 引擎。
func NewApp(
	st *store.Store,
	bl *blobs.Store,
	w *clipwatch.Watcher,
	p *paster.Paster,
	hk *hotkey.Manager,
	mw *winapi.MessageWindow,
	ex *exclusion.List,
	settings model.Settings,
	ocrEngine *ocr.Engine,
) *App {
	return &App{
		store:     st,
		blobs:     bl,
		watcher:   w,
		paster:    p,
		hotkeys:   hk,
		msgwin:    mw,
		excluded:  ex,
		settings:  settings,
		ocrEngine: ocrEngine,
		ocrQueue:  make(chan store.PendingOCR, 256),
		ocrStop:   make(chan struct{}),
	}
}

// Startup 在 Wails 初始化完成后调用,此时 ctx 可用。
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	log.Println("PasteWin 已启动")

	// 对齐自启状态。用 Reconcile 而非 Sync:安装程序或 Windows 的
	// 「启动应用」设置可能已经改过注册表,那些改动以注册表为准。
	if enabled, err := autostart.Reconcile(a.settings.AutoStart); err != nil {
		log.Printf("同步开机自启状态失败: %v", err)
	} else if enabled != a.settings.AutoStart {
		// 外部来源开启的自启,回写到配置,避免界面显示与实际不符
		log.Printf("检测到外部开启的自启项,已同步到配置")
		a.mu.Lock()
		next := a.settings
		next.AutoStart = enabled
		a.settings = next
		a.mu.Unlock()
		if err := a.store.SaveSettings(next); err != nil {
			log.Printf("回写自启配置失败: %v", err)
		}
	}

	if err := a.registerHotkey(a.settings.Hotkey); err != nil {
		log.Printf("注册全局热键失败: %v", err)
		a.emitError(fmt.Sprintf("热键 %s 注册失败,可能已被其他程序占用", a.settings.Hotkey))
	}

	a.startOCRWorker()
}

// ---------------------------------------------------------------- 后台 OCR

// startOCRWorker 启动 OCR 工作协程,并把历史图片排进队列。
func (a *App) startOCRWorker() {
	if a.ocrEngine == nil {
		log.Println("OCR: 本机没有可用引擎,图片文字搜索不可用")
		return
	}

	a.ocrWG.Add(1)
	go func() {
		defer a.ocrWG.Done()
		for {
			select {
			case <-a.ocrStop:
				return
			case job := <-a.ocrQueue:
				a.runOCR(job)
			}
		}
	}()

	// 识别流水线升级后,已识别过的图片要用新流水线重做一遍 ——
	// 老结果停在旧质量上(比如加入放大前的识别结果会把「陈」拆成「阝东」)。
	// 用版本标记而不是"每次启动都重做":后者等于每次开机重跑几十个
	// PowerShell 进程。
	cur, _, err := a.store.Meta(ocrPipelineKey)
	if err != nil {
		log.Printf("OCR: 读取流水线版本失败: %v", err)
	} else if cur != ocr.PipelineVersion {
		n, err := a.store.ResetOCR()
		if err != nil {
			log.Printf("OCR: 重置识别状态失败: %v", err)
		} else {
			if n > 0 {
				log.Printf("OCR: 流水线已更新(%s → %s),%d 张图片将重新识别",
					orDash(cur), ocr.PipelineVersion, n)
			}
			if err := a.store.SetMeta(ocrPipelineKey, ocr.PipelineVersion); err != nil {
				log.Printf("OCR: 写入流水线版本失败: %v", err)
			}
		}
	}

	// 历史图片补做。只取一批:剩下的等下次启动继续,避免开机瞬间
	// 有几十个 PowerShell 进程排队把机器拖慢。
	pending, err := a.store.ImagesNeedingOCR(ocrBackfillPerLaunch)
	if err != nil {
		log.Printf("OCR: 查询历史图片失败: %v", err)
		return
	}
	for _, job := range pending {
		a.enqueueOCR(job)
	}
	if len(pending) > 0 {
		log.Printf("OCR: 已排入 %d 张历史图片待识别", len(pending))
	}
}

// enqueueOCR 把一张图片投入识别队列。队列满时直接丢弃 ——
// 识别只是锦上添花,不能因为队列积压就阻塞捕获路径。
func (a *App) enqueueOCR(job store.PendingOCR) {
	select {
	case a.ocrQueue <- job:
	default:
		log.Printf("OCR: 队列已满,跳过 %s", job.Path)
	}
}

// runOCR 识别一张图片并写回结果。在单个工作协程上串行执行。
func (a *App) runOCR(job store.PendingOCR) {
	// 条目可能在排队期间被删掉了,或者用户清空了历史
	abs, err := a.blobs.Abs(job.Path)
	if err != nil {
		log.Printf("OCR: 路径非法 %s: %v", job.Path, err)
		return
	}

	text, err := a.ocrEngine.Recognize(a.ctx, abs)
	if err != nil {
		if errors.Is(err, ocr.ErrNoEngine) {
			log.Printf("OCR: 本机没有可用引擎,停止识别")
			return
		}
		// 单张失败不标记 ocr_done,下次启动会重试
		log.Printf("OCR: 识别 %s 失败: %v", job.Path, err)
		return
	}

	if err := a.store.SetOCRText(job.ID, text); err != nil {
		// 条目很可能已被删除,属于正常情况,不必刷屏
		log.Printf("OCR: 写回结果失败(%s): %v", job.Path, err)
		return
	}
	log.Printf("OCR: %s 识别出 %d 字", job.Path, len([]rune(text)))

	// 面板可能正开着,让搜索结果实时跟上
	a.emitItemsChanged()
}

// stopOCR 停掉工作协程并等它退出。
func (a *App) stopOCR() {
	select {
	case <-a.ocrStop:
		return // 已经停过
	default:
	}
	close(a.ocrStop)
	a.ocrWG.Wait()
}

// Shutdown 在应用退出前调用。与 Quit 的强制退出路径共用同一份清理逻辑。
func (a *App) Shutdown(ctx context.Context) {
	log.Println("正在退出")
	a.Cleanup()
}

// ---------------------------------------------------------------- 热键与面板

func (a *App) registerHotkey(accel string) error {
	parsed, err := hotkey.Parse(accel)
	if err != nil {
		return err
	}
	id, err := a.hotkeys.Register(parsed, a.TogglePanel)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.hotkeyID = id
	a.mu.Unlock()
	log.Printf("已注册全局热键 %s (id=%d)", accel, id)
	return nil
}

// TogglePanel 切换面板显隐。由全局热键触发。
func (a *App) TogglePanel() {
	a.mu.Lock()
	shown := a.panelShown
	a.mu.Unlock()

	if shown {
		a.HidePanel()
		return
	}
	a.ShowPanel()
}

// ShowPanel 在鼠标所在显示器底部弹出面板。
func (a *App) ShowPanel() {
	a.mu.Lock()
	if a.panelShown {
		a.mu.Unlock()
		return
	}
	// 必须在面板获得焦点之前记录,否则拿到的就是 PasteWin 自己
	a.prevTarget = winapi.CaptureFocusTarget()
	a.panelShown = true
	height := a.settings.PanelHeight
	a.mu.Unlock()

	mon, err := winapi.MonitorAtCursor()
	if err != nil {
		log.Printf("获取显示器工作区失败,回退到主屏: %v", err)
	}

	if a.ctx != nil {
		if err == nil {
			a.applyPanelGeometry(mon, height)
		}
		runtime.WindowShow(a.ctx)
		runtime.WindowSetAlwaysOnTop(a.ctx, true)
		runtime.EventsEmit(a.ctx, "panel:shown")

		// Wails 的 WindowShow 不保证把窗口提到前台:当前有别的窗口占着焦点时,
		// 它可能只是显示出来而没有激活,用户按的 Enter 会落到原来的窗口上。
		// 这里显式抢一次焦点,否则面板看得见却没法用键盘操作。
		a.focusPanel()

		log.Printf("面板已弹出(目标窗口 %d(%s) 控件 %d(%s) 当前前台 %d)",
			a.prevTarget.Window, winapi.ClassName(a.prevTarget.Window),
			a.prevTarget.Focus, winapi.ClassName(a.prevTarget.Focus),
			winapi.GetForegroundWindow())
	}
}

// applyPanelGeometry 把面板摆到 mon 这块显示器的底部。
//
// logicalHeight 是**逻辑像素**高度(用户设的偏好),乘上该显示器的缩放
// 换算成物理像素,这样面板在 100% 和 125% 的屏幕上看起来一样高。
// 宽度则直接用工作区的物理宽度铺满,不做缩放 —— 缩放会让面板越过屏幕边缘。
func (a *App) applyPanelGeometry(mon winapi.Monitor, logicalHeight int) {
	hwnd := winapi.FindWindowByTitle(mainWindowTitle)
	if hwnd == 0 {
		log.Printf("未能按标题找到主窗口,面板无法定位")
		return
	}

	w := int(mon.Work.Width())
	h := int(float64(logicalHeight)*mon.Scale() + 0.5)
	if h > int(mon.Work.Height()) {
		h = int(mon.Work.Height())
	}
	x := int(mon.Work.Left)
	y := int(mon.Work.Bottom) - h

	if err := winapi.SetWindowBounds(hwnd, x, y, w, h); err != nil {
		log.Printf("面板定位失败: %v", err)
		return
	}
	log.Printf("面板几何: 工作区 %d,%d → %d,%d(%d×%d) 缩放 %.2f 逻辑高 %d → 物理 %d×%d @%d,%d",
		mon.Work.Left, mon.Work.Top, mon.Work.Right, mon.Work.Bottom,
		mon.Work.Width(), mon.Work.Height(), mon.Scale(), logicalHeight, w, h, x, y)
	a.logPanelRect()

	// 面板跨到缩放比例不同的显示器上时,Windows 会补一条 WM_DPICHANGED,
	// 而 Wails 会照单全收它建议的矩形(windows/window.go 的 0x02E0 分支)——
	// 那个建议值是按新旧 DPI 之比缩放出来的,会把我们算好的物理尺寸改掉:
	// 1920×280 从 125% 的屏挪到 100% 的屏上会变成 1536×224。
	// 这条消息是异步到的,只能等它落地后回读校验、不符再纠一次。
	want := winapi.RECT{Left: int32(x), Top: int32(y), Right: int32(x + w), Bottom: int32(y + h)}
	a.mu.Lock()
	a.panelBounds = want
	a.mu.Unlock()
	go a.settlePanelBounds(hwnd, want)
}

// settlePanelBounds 回读面板矩形,被 WM_DPICHANGED 改掉就重新设回去。
//
// 放在 goroutine 里:面板已经显示出来了,纠偏晚一两帧用户看不出来,
// 但同步等会让每次唤起都卡一下。
func (a *App) settlePanelBounds(hwnd uintptr, want winapi.RECT) {
	for i := 0; i < panelSettleTries; i++ {
		time.Sleep(panelSettleInterval)

		// 用户可能已经把面板收起来了,别再动窗口
		a.mu.Lock()
		shown := a.panelShown
		a.mu.Unlock()
		if !shown {
			return
		}

		got, err := winapi.WindowRect(hwnd)
		if err != nil {
			return
		}
		if got == want {
			return
		}
		if err := winapi.SetWindowBounds(hwnd,
			int(want.Left), int(want.Top), int(want.Width()), int(want.Height())); err != nil {
			log.Printf("面板几何: 纠偏失败 %v", err)
			return
		}
		log.Printf("面板几何: 被 WM_DPICHANGED 改成 %d,%d → %d,%d(%d×%d),已纠回",
			got.Left, got.Top, got.Right, got.Bottom, got.Width(), got.Height())
	}
	log.Printf("面板几何: 纠偏 %d 次仍未稳定,放弃", panelSettleTries)
}

// StartPanelResize 开始一次"拖拽面板上边缘改高"。
//
// 由前端在按下顶部把手时调用,返回时拖拽已结束。
// 整个拖拽在 Go 侧轮询鼠标左键状态完成,而不是每次 mousemove 都过一次
// JS↔Go 桥 —— 后者在拖拽这种高频场景下会明显发涩。
//
// 面板贴屏幕底边,所以高度就是"工作区底边 - 光标 Y"。拖拽期间光标可能
// 甩出工作区,按边界钳住即可,不需要中断。
func (a *App) StartPanelResize() {
	a.mu.Lock()
	if a.resizing {
		a.mu.Unlock()
		return
	}
	a.resizing = true
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.resizing = false
		a.mu.Unlock()
	}()

	hwnd := winapi.FindWindowByTitle(mainWindowTitle)
	if hwnd == 0 {
		return
	}
	mon, err := winapi.MonitorOfWindow(hwnd)
	if err != nil {
		log.Printf("拖拽改高: 取显示器失败 %v", err)
		return
	}

	scale := mon.Scale()
	bottom := int(mon.Work.Bottom)
	x := int(mon.Work.Left)
	width := int(mon.Work.Width())

	minH := int(float64(minPanelLogicalHeight)*scale + 0.5)
	maxH := int(float64(maxPanelLogicalHeight)*scale + 0.5)
	if maxH > int(mon.Work.Height()) {
		maxH = int(mon.Work.Height())
	}

	for winapi.IsLeftButtonDown() {
		_, cy, err := winapi.CursorPos()
		if err != nil {
			break
		}
		h := bottom - int(cy)
		if h < minH {
			h = minH
		}
		if h > maxH {
			h = maxH
		}
		if err := winapi.SetWindowBounds(hwnd, x, bottom-h, width, h); err != nil {
			log.Printf("拖拽改高: 设置窗口失败 %v", err)
			break
		}
		time.Sleep(resizePollInterval)
	}

	// 落库时换算回逻辑像素,这样换到别的缩放比例的显示器上高度观感一致
	r, err := winapi.WindowRect(hwnd)
	if err != nil {
		return
	}
	a.mu.Lock()
	a.panelBounds = r
	a.mu.Unlock()
	logical := int(float64(r.Height())/scale + 0.5)
	a.savePanelHeight(logical)
}

// savePanelHeight 记录用户拖出来的面板高度(逻辑像素)。
//
// 高度是用户偏好,必须落库 —— 否则每次重新呼出面板都会被
// ShowPanel 按旧值重置回去,用户会觉得"调了等于没调"。
func (a *App) savePanelHeight(logical int) {
	a.mu.Lock()
	if a.settings.PanelHeight == logical {
		a.mu.Unlock()
		return
	}
	next := a.settings
	next.PanelHeight = logical
	a.settings = next
	a.mu.Unlock()

	if err := a.store.SaveSettings(next); err != nil {
		log.Printf("保存面板高度失败: %v", err)
		return
	}
	log.Printf("面板高度已保存: %d(逻辑像素)", logical)
}

// persistPanelHeight 把面板当前的实际高度写回配置。
//
// 兜底用:用户也可能直接拖窗口边框改高(无边框窗口仍带 WS_THICKFRAME),
// 那条路径不经过 StartPanelResize。
//
// **只在矩形确实变过时才保存**。无条件回读并落库会形成"垃圾进垃圾出":
// 窗口尺寸在跨显示器时可能被 WM_DPICHANGED 短暂改坏,一旦被当成用户偏好
// 存下来,之后每次唤起都会按这个坏值渲染。
func (a *App) persistPanelHeight() {
	hwnd := winapi.FindWindowByTitle(mainWindowTitle)
	if hwnd == 0 {
		return
	}

	a.mu.Lock()
	applied := a.panelBounds
	a.mu.Unlock()
	if applied == (winapi.RECT{}) {
		// 本次还没定位过,没有可比对的基准
		return
	}

	r, err := winapi.WindowRect(hwnd)
	if err != nil || r == applied {
		return
	}

	mon, err := winapi.MonitorOfWindow(hwnd)
	if err != nil {
		return
	}
	scale := mon.Scale()
	if scale <= 0 {
		return
	}
	a.savePanelHeight(int(float64(r.Height())/scale + 0.5))
}

// logPanelRect 回读面板的实际窗口矩形并打日志。
//
// SetWindowPos 是同步的,调用返回即可读到新值;这一步是为了在日志里留下
// "实际摆到哪儿了"的证据 —— 多屏 + 混合缩放下出问题时,这是唯一能区分
// "算错了"和"没生效"的东西。
func (a *App) logPanelRect() {
	hwnd := winapi.FindWindowByTitle(mainWindowTitle)
	if hwnd == 0 {
		return
	}
	r, err := winapi.WindowRect(hwnd)
	if err != nil {
		log.Printf("面板几何: 回读失败 %v", err)
		return
	}
	log.Printf("面板几何: 实际 %d,%d → %d,%d(%d×%d)",
		r.Left, r.Top, r.Right, r.Bottom, r.Width(), r.Height())
}

// focusPanel 把 PasteWin 主窗口提到前台并交出键盘焦点。
//
// 必须等窗口真正显示出来再抢前台。runtime.WindowShow 只是把"显示"投递到
// 主线程执行,返回时窗口往往还是隐藏状态;而对一个隐藏窗口调
// SetForegroundWindow 必然失败(返回 0,前台窗口纹丝不动)。
// 这曾表现为"面板时好时坏地拿不到键盘焦点"。
func (a *App) focusPanel() {
	hwnd := winapi.FindWindowByTitle(mainWindowTitle)
	if hwnd == 0 {
		log.Printf("未能按标题找到主窗口,面板可能无法接收键盘输入")
		return
	}

	// 轮询等它可见。没有现成的完成信号,只能等;超时上限很短,最坏多花几百毫秒。
	deadline := time.Now().Add(focusWaitTimeout)
	for !winapi.IsWindowVisible(hwnd) {
		if time.Now().After(deadline) {
			log.Printf("主窗口在 %v 内仍未显示,放弃抢前台", focusWaitTimeout)
			return
		}
		time.Sleep(focusPollInterval)
	}

	if err := winapi.ForceForeground(hwnd); err != nil {
		log.Printf("面板抢焦点失败: %v", err)
	}
}

// HidePanel 收起面板,并把焦点还给唤起前的窗口。
//
// 由用户主动收起(按 Esc、再按一次热键)时走这里。把焦点还回去是必要的:
// 面板一隐藏,系统就没有前台窗口了,用户的后续输入会无处可去。
// 回贴流程不走这里 —— 它自己会把焦点交给目标窗口,见 PasteItem。
func (a *App) HidePanel() {
	a.mu.Lock()
	target := a.prevTarget
	a.mu.Unlock()

	if !a.hidePanelWindow() {
		return
	}
	if target.Window != 0 {
		if err := target.Restore(); err != nil {
			log.Printf("收起面板时还原焦点失败: %v", err)
		} else {
			log.Printf("收起面板: 焦点已还给 %d(%s),当前前台=%d",
				target.Window, winapi.ClassName(target.Window), winapi.GetForegroundWindow())
		}
	}
}

// hidePanelWindow 只隐藏窗口,不碰焦点。返回是否确实执行了隐藏。
func (a *App) hidePanelWindow() bool {
	a.mu.Lock()
	if !a.panelShown {
		a.mu.Unlock()
		return false
	}
	a.panelShown = false
	a.mu.Unlock()

	if a.ctx != nil {
		runtime.WindowHide(a.ctx)
		runtime.EventsEmit(a.ctx, "panel:hidden")
		log.Printf("面板已收起")
		// 收起时统一回读高度:拖把手改高走 StartPanelResize,
		// 但用户也可能直接拖无边框窗口的边框,那条路径不落库。
		go a.persistPanelHeight()
	}
	return true
}

// ---------------------------------------------------------------- 列表查询

// ListItems 按条件查询条目,供面板渲染卡片流。
func (a *App) ListItems(f model.Filter) ([]model.Item, error) {
	items, err := a.store.List(f)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []model.Item{}
	}
	return items, nil
}

// GetItem 取单条。
func (a *App) GetItem(id string) (model.Item, error) { return a.store.Get(id) }

// CountItems 返回条目总数,用于界面显示。
func (a *App) CountItems() (int, error) { return a.store.Count() }

// SourceApps 返回历史中出现过的来源应用,供过滤下拉使用。
func (a *App) SourceApps() ([]string, error) { return a.store.DistinctSourceApps() }

// GetItemText 返回条目的纯文本内容,供"就地编辑"填入编辑框。
func (a *App) GetItemText(id string) (string, error) {
	it, err := a.store.Get(id)
	if err != nil {
		return "", err
	}
	if it.Kind == model.KindImage {
		return "", fmt.Errorf("图片条目不支持文本编辑")
	}
	if it.PlainText != "" {
		return it.PlainText, nil
	}
	return it.Content, nil
}

// ---------------------------------------------------------------- 条目操作

// DeleteItem 删除条目,并清理其图片文件。
func (a *App) DeleteItem(id string) error {
	blob, thumb, err := a.store.Delete(id)
	if err != nil {
		return err
	}
	// 文件清理失败不影响删除结果,只记日志 —— 库里的记录已经没了,
	// 向用户报错反而会让人以为删除失败
	if err := a.blobs.RemoveAll([]string{blob, thumb}); err != nil {
		log.Printf("清理图片文件失败: %v", err)
	}
	a.emitItemsChanged()
	return nil
}

// TogglePin 切换置顶。
func (a *App) TogglePin(id string) (bool, error) {
	pinned, err := a.store.TogglePin(id)
	if err != nil {
		return false, err
	}
	a.emitItemsChanged()
	return pinned, nil
}

// UpdateItemContent 就地编辑条目文本。
func (a *App) UpdateItemContent(id, content string) error {
	if err := a.store.UpdateContent(id, content); err != nil {
		return err
	}
	a.emitItemsChanged()
	return nil
}

// ---------------------------------------------------------------- 回贴

// PasteItem 回贴单条到唤起面板前的窗口。
func (a *App) PasteItem(id string, plainText bool) error {
	it, err := a.store.Get(id)
	if err != nil {
		return err
	}

	a.mu.Lock()
	target := a.prevTarget
	a.mu.Unlock()

	// 注意:这里**不能**先把面板藏起来再交给 paster。
	// 面板还在前台时本进程才是"前台进程",SetForegroundWindow 才会被真正执行;
	// 先藏面板会让前台变成空,之后谁也抢不回来。顺序由 paster.handOver 保证。
	if err := a.paster.Paste(target, it, plainText, func() { a.hidePanelWindow() }); err != nil {
		log.Printf("回贴失败: %v", err)
		a.emitError(err.Error())
		return err
	}
	return nil
}

// CopyItem 只把条目复制到剪贴板,不注入按键。
func (a *App) CopyItem(id string, plainText bool) error {
	it, err := a.store.Get(id)
	if err != nil {
		return err
	}
	// 写入剪贴板前会置防回环标记,因此不会产生新条目,无需通知前端刷新
	return a.paster.WriteToClipboard(it, plainText)
}

// PasteStack 把队列中的条目按顺序逐条粘贴,每条之间留出间隔。
//
// 注:这是"按顺序逐条粘贴",与 Paste 的交互式粘贴栈(每次按 Cmd+V 弹出下一条)
// 不同 —— 后者需要在粘贴栈激活期间用低级键盘钩子拦截 Ctrl+V 才能实现。
func (a *App) PasteStack(ids []string) error {
	if len(ids) == 0 {
		return errors.New("粘贴队列为空")
	}

	a.mu.Lock()
	target := a.prevTarget
	a.mu.Unlock()

	// 先把条目取齐,再交给 paster 一次完成焦点交还与逐条注入 ——
	// 焦点必须在收起面板之前交还,见 paster.handOver。
	items := make([]model.Item, 0, len(ids))
	var missing []string
	for _, id := range ids {
		it, err := a.store.Get(id)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return fmt.Errorf("粘贴队列为空")
	}

	err := a.paster.PasteSequence(target, items, func() { a.hidePanelWindow() })
	if len(missing) > 0 {
		log.Printf("顺序粘贴: %d 条取不到: %s", len(missing), strings.Join(missing, "; "))
	}
	return err
}

// ---------------------------------------------------------------- 分组

// ListPinboards 返回全部分组。
func (a *App) ListPinboards() ([]model.Pinboard, error) {
	boards, err := a.store.ListPinboards()
	if err != nil {
		return nil, err
	}
	if boards == nil {
		boards = []model.Pinboard{}
	}
	return boards, nil
}

// CreatePinboard 新建分组。
func (a *App) CreatePinboard(name string) (model.Pinboard, error) {
	b, err := a.store.CreatePinboard(name)
	if err != nil {
		return model.Pinboard{}, err
	}
	a.emitItemsChanged()
	return b, nil
}

// RenamePinboard 重命名分组。
func (a *App) RenamePinboard(id, name string) error {
	if err := a.store.RenamePinboard(id, name); err != nil {
		return err
	}
	a.emitItemsChanged()
	return nil
}

// DeletePinboard 删除分组(组内条目保留)。
func (a *App) DeletePinboard(id string) error {
	if err := a.store.DeletePinboard(id); err != nil {
		return err
	}
	a.emitItemsChanged()
	return nil
}

// MoveToPinboard 把条目移入分组;pinboardID 为空表示移出。
func (a *App) MoveToPinboard(itemID, pinboardID string) error {
	if err := a.store.MoveToPinboard(itemID, pinboardID); err != nil {
		return err
	}
	a.emitItemsChanged()
	return nil
}

// ---------------------------------------------------------------- 配置

// GetSettings 返回当前配置。
func (a *App) GetSettings() model.Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings
}

// UpdateSettings 保存配置,并按需应用副作用(热键、自启、排除名单)。
func (a *App) UpdateSettings(next model.Settings) error {
	a.mu.Lock()
	prev := a.settings
	a.settings = next
	a.mu.Unlock()

	if err := a.store.SaveSettings(next); err != nil {
		// 保存失败就回滚内存状态,避免界面显示的与落盘的不一致
		a.mu.Lock()
		a.settings = prev
		a.mu.Unlock()
		return err
	}

	if a.tray != nil {
		a.tray.SetPaused(next.PauseCapture)
	}
	a.excluded.Replace(next.ExcludedApps)
	a.watcher.SetPaused(next.PauseCapture)

	if next.Hotkey != prev.Hotkey {
		if err := a.changeHotkey(prev.Hotkey, next.Hotkey); err != nil {
			return err
		}
	}
	if next.AutoStart != prev.AutoStart {
		if err := autostart.Sync(next.AutoStart); err != nil {
			return fmt.Errorf("设置开机自启失败: %w", err)
		}
	}
	return nil
}

// changeHotkey 换绑全局热键。新组合注册失败时回退到旧组合,
// 避免用户改成一个被占用的组合后连面板都唤不出来。
func (a *App) changeHotkey(oldAccel, newAccel string) error {
	a.mu.Lock()
	oldID := a.hotkeyID
	a.mu.Unlock()

	if oldID != 0 {
		if err := a.hotkeys.Unregister(oldID); err != nil {
			log.Printf("注销旧热键失败: %v", err)
		}
		a.mu.Lock()
		a.hotkeyID = 0
		a.mu.Unlock()
	}

	if err := a.registerHotkey(newAccel); err != nil {
		log.Printf("新热键 %s 注册失败,回退到 %s: %v", newAccel, oldAccel, err)
		if rbErr := a.registerHotkey(oldAccel); rbErr != nil {
			return fmt.Errorf("热键 %s 注册失败,回退到 %s 也失败: %w", newAccel, oldAccel, rbErr)
		}
		a.mu.Lock()
		a.settings.Hotkey = oldAccel
		a.mu.Unlock()
		if saveErr := a.store.SaveSettings(a.GetSettings()); saveErr != nil {
			log.Printf("回滚热键配置失败: %v", saveErr)
		}
		return fmt.Errorf("热键 %s 注册失败(可能被占用),已保留原热键 %s", newAccel, oldAccel)
	}
	return nil
}

// SetPaused 暂停/恢复剪贴板捕获。
func (a *App) SetPaused(paused bool) error {
	a.mu.Lock()
	next := a.settings
	next.PauseCapture = paused
	a.mu.Unlock()
	return a.UpdateSettings(next)
}

// OpenDataDir 在资源管理器中打开数据目录。
func (a *App) OpenDataDir() {
	if a.ctx == nil {
		return
	}
	if err := os.MkdirAll(a.store.Dir(), 0o755); err != nil {
		a.emitError(fmt.Sprintf("打开数据目录失败: %v", err))
		return
	}
	runtime.BrowserOpenURL(a.ctx, "file://"+filepath.ToSlash(a.store.Dir()))
}

// SetTray 注入托盘实例,用于同步暂停状态与提示文字。
func (a *App) SetTray(t *tray.Tray) {
	a.mu.Lock()
	a.tray = t
	paused := a.settings.PauseCapture
	a.mu.Unlock()
	t.SetPaused(paused)
}

// HandleTrayCommand 处理托盘菜单与左键点击。由 main 的消息路由调用。
func (a *App) HandleTrayCommand(cmd uint32) {
	switch cmd {
	case tray.CmdTogglePanel:
		a.TogglePanel()

	case tray.CmdPauseToggle:
		a.mu.Lock()
		next := !a.settings.PauseCapture
		a.mu.Unlock()
		if err := a.SetPaused(next); err != nil {
			log.Printf("切换暂停状态失败: %v", err)
			a.emitError(fmt.Sprintf("切换暂停状态失败: %v", err))
		}

	case tray.CmdSettings:
		// 先确保面板可见,再让前端打开设置弹层
		a.ShowPanel()
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "panel:settings")
		}

	case tray.CmdOpenDataDir:
		a.OpenDataDir()

	case tray.CmdQuit:
		a.Quit()
	}
}

// quitGracePeriod 是等待 Wails 优雅退出的宽限期,超时后强制退出。
const (
	// focusWaitTimeout 是"等主窗口显示出来"的上限。
	// 窗口显示通常在一帧内完成,超时给得宽松只是为了避免卡住调用方。
	focusWaitTimeout = 500 * time.Millisecond
	// focusPollInterval 是等待窗口可见时的轮询间隔。
	focusPollInterval = 10 * time.Millisecond

	// ocrPipelineKey 是记录当前 OCR 流水线版本的设置键。
	ocrPipelineKey = "ocr_pipeline_version"

	// ocrBackfillPerLaunch 限制每次启动补做的历史图片数量。
	// 每张要起一个 PowerShell 进程(约 1.5 秒),一次排太多会明显拖慢开机。
	ocrBackfillPerLaunch = 20

	quitGracePeriod = 1500 * time.Millisecond

	// 面板高度的逻辑像素上下限。必须与 store.normalize 的钳位一致 ——
	// 两处不一致会出现"拖到极限、重开又弹回"这种自相矛盾的行为。
	minPanelLogicalHeight = 160
	maxPanelLogicalHeight = 900

	// resizePollInterval 是拖拽改高时重设窗口的间隔。
	// 8ms 约合 120Hz,再快也没有意义(屏幕刷不过来)。
	resizePollInterval = 8 * time.Millisecond

	// 面板跨显示器后对 WM_DPICHANGED 的纠偏重试。
	// 该消息通常一帧内到,一两轮就稳定;给足余量但不无限重试。
	panelSettleTries    = 8
	panelSettleInterval = 25 * time.Millisecond
)

// Quit 退出应用。
//
// 必须先把 quitting 置位:Wails 的 Quit 会先调用 OnBeforeClose,
// 而那个钩子为了"点 X 只收起不退出"会返回 true,从而把程序化退出也挡掉。
//
// 另外还加了超时兜底。Wails 的 runtime.Quit 走的是"给主线程投递退出消息"
// 的路径(见 wails/v2/internal/frontend/desktop/windows/frontend.go 的
// Frontend.Quit),实测在本机它返回了但主消息循环并未终止 ——
// 表现就是点了「退出」毫无反应。这里不依赖那条路径是否可靠:
// 宽限期内没退干净就自己清理再硬退出,保证退出对用户永远有效。
func (a *App) Quit() {
	if a.quitting.Swap(true) {
		return // 已经在退出了
	}

	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}

	go func() {
		time.Sleep(quitGracePeriod)
		log.Printf("宽限期 %v 内未退出,执行强制退出", quitGracePeriod)
		a.Cleanup()
		os.Exit(0)
	}()
}

// Cleanup 释放资源。可重复调用,内部用 Once 保证只执行一次。
func (a *App) Cleanup() {
	a.cleanupOnce.Do(func() {
		// 先停 OCR:它持有数据库连接,必须在关库之前收尾
		a.stopOCR()
		if a.tray != nil {
			a.tray.Close()
		}
		a.mu.Lock()
		id := a.hotkeyID
		a.hotkeyID = 0
		a.mu.Unlock()
		if id != 0 {
			if err := a.hotkeys.Unregister(id); err != nil {
				log.Printf("注销热键失败: %v", err)
			}
		}
		if a.msgwin != nil {
			winapi.RemoveClipboardFormatListener(a.msgwin.HWND())
		}
		if a.store != nil {
			if err := a.store.Close(); err != nil {
				log.Printf("关闭数据库失败: %v", err)
			}
		}
	})
}

// IsQuitting 报告是否正在退出。OnBeforeClose 用它决定要不要放行。
func (a *App) IsQuitting() bool { return a.quitting.Load() }

// ---------------------------------------------------------------- 内部工具

// OnCaptured 由剪贴板监听器回调,负责落库并通知前端。
func (a *App) OnCaptured(c model.ClipContent) {
	blobPath, thumbPath := "", ""

	if c.Kind == model.KindImage && len(c.ImagePNG) > 0 {
		// 文件名取自内容 hash,即按内容寻址。
		// 这样做是安全的:Insert 按 hash 去重,同一个 hash 只可能对应一条记录,
		// 不存在两个条目共用一个文件、删其一导致另一个图片丢失的情况。
		name := c.Hash
		if len(name) > 32 {
			name = name[:32]
		}
		var err error
		blobPath, err = a.blobs.SaveImage(name, c.ImagePNG)
		if err != nil {
			log.Printf("保存图片失败: %v", err)
		} else if thumbPath, err = a.blobs.MakeThumb(name, c.ImagePNG); err != nil {
			log.Printf("生成缩略图失败: %v", err)
		}
	}

	it, isNew, err := a.store.Insert(c, blobPath, thumbPath)
	if err != nil {
		log.Printf("保存剪贴板内容失败: %v", err)
		return
	}
	if !isNew {
		// 重复内容只是被提到了最前,刷新列表即可
		log.Printf("重复内容,已提前: %s", truncate(it.Preview, 40))
		a.emitItemsChanged()
		return
	}

	log.Printf("捕获 %s: %q (来源 %s, %d 字节)",
		it.Kind, truncate(it.Preview, 40), orUnknown(it.SourceApp), it.ByteSize)

	// 新图片投入后台识别队列,让截图里的文字也能被搜到。
	// 只处理新建的条目:重复内容只是被提到最前,早已识别过。
	if isNew && it.Kind == model.KindImage && it.BlobPath != "" {
		a.enqueueOCR(store.PendingOCR{ID: it.ID, Path: it.BlobPath})
	}

	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "clip:new", it)
	}
	a.pruneIfNeeded()
}

// truncate 按字符截断,避免把整段内容写进日志。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func orUnknown(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

// orDash 用于日志里表示"首次运行、标记还不存在"。
func orDash(s string) string {
	if s == "" {
		return "无"
	}
	return s
}

// pruneIfNeeded 按配置裁剪历史。
func (a *App) pruneIfNeeded() {
	a.mu.Lock()
	maxItems := a.settings.MaxItems
	a.mu.Unlock()

	res, err := a.store.Prune(maxItems)
	if err != nil {
		log.Printf("清理历史失败: %v", err)
		return
	}
	if res.Count == 0 {
		return
	}
	if err := a.blobs.RemoveAll(res.DeletedBlobs); err != nil {
		log.Printf("清理图片文件失败: %v", err)
	}
	log.Printf("已清理 %d 条过期历史", res.Count)
}

func (a *App) emitItemsChanged() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "items:changed")
	}
}

func (a *App) emitError(msg string) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "error", msg)
	}
}

// ClientLog 接收前端的诊断日志,写进应用日志。
//
// 面板跑在 WebView2 里,没有可见的控制台;出问题时只能靠这条通道把
// 前端状态带出来。属于长期保留的排障设施,不是临时调试代码。
func (a *App) ClientLog(msg string) {
	log.Printf("[前端] %s", msg)
}

// DebugState 返回内部状态快照,供排障使用。
func (a *App) DebugState() (string, error) {
	a.mu.Lock()
	s := a.settings
	shown := a.panelShown
	prev := a.prevTarget
	a.mu.Unlock()

	n, _ := a.store.Count()
	state := map[string]any{
		"dataDir":      a.store.Dir(),
		"itemCount":    n,
		"panelShown":   shown,
		"prevTarget":   prev,
		"hotkey":       s.Hotkey,
		"pauseCapture": s.PauseCapture,
		"excluded":     a.excluded.Apps(),
	}
	out, err := json.MarshalIndent(state, "", "  ")
	return string(out), err
}
