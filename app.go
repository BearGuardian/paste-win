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

	area, err := winapi.WorkAreaAtCursor()
	if err != nil {
		log.Printf("获取显示器工作区失败,回退到屏幕底部: %v", err)
	}

	if a.ctx != nil {
		if err == nil {
			w := int(area.Width())
			h := height
			if h > int(area.Height()) {
				h = int(area.Height())
			}
			runtime.WindowSetSize(a.ctx, w, h)
			runtime.WindowSetPosition(a.ctx, int(area.Left), int(area.Bottom)-h)
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
