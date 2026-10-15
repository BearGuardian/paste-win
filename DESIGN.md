# PasteWin 设计文档

> Windows 版 Paste 剪贴板管理器
> 状态:设计已评审通过,进入实施
> 日期:2026-10-15

---

## 1. 背景与目标

### 1.1 背景

macOS 的 **Paste**(pasteapp.io)把系统单条剪贴板扩展成可搜索、可置顶、可分组的历史库:按 `⇧⌘V` 从屏幕底部滑出卡片面板,回车即回贴到唤起前的窗口。

Windows 自带的 `Win+V` 剪贴板历史存在明显差距:

- 只有纯文本和图片,不支持文件列表、富文本
- 无 Pinboard 分组,无置顶
- **无焦点还原**——选中后不会自动粘回原窗口
- UI 简陋,不可搜索来源应用/日期

### 1.2 目标

在 Windows 11 上复刻 Paste 的核心体验,产出常驻托盘的单文件 exe:

`Ctrl+Shift+V` 唤出底部卡片面板 → 搜索/方向键浏览 → 回车回贴到唤起前的窗口

### 1.3 非目标

- iCloud 跨设备同步
- OCR 搜索(截图内文字)
- 团队共享 Pinboard
- 移动端

---

## 2. 环境事实

均为本机实测结论,非推断。

| 项 | 结论 |
|---|---|
| Go | **1.27.0**,位于 `D:\code-env\.tools\go\bin\go`,**不在 PATH**,需显式 export |
| `CGO_ENABLED` | **0** → SQLite 必须用纯 Go 驱动 `modernc.org/sqlite` |
| `proxy.golang.org` | **不通**(curl 返回 000) |
| `goproxy.cn` | **通**(200),必须设 `GOPROXY=https://goproxy.cn,direct` |
| WebView2 Runtime | **已安装** `C:\Program Files (x86)\Microsoft\EdgeWebView\Application\155.0.4283.45` |
| Node | v24.15.0,位于 `D:\code-env\nodejs\` |
| .NET SDK / MSVC | 均无 |
| Go module cache | 近乎空,所有依赖需联网拉取 |

### 2.1 依赖可用性

已在 goproxy.cn 上实测返回版本号:

| module | 版本 | 用途 |
|---|---|---|
| `github.com/wailsapp/wails/v2` | v2.16.0 | 窗口 + WebView2 + JS 桥 + 资源内嵌 |
| `modernc.org/sqlite` | v1.60.1 | 纯 Go SQLite(CGO_ENABLED=0 下唯一选择) |
| `golang.org/x/sys` | v0.49.0 | Win32 syscall:剪贴板 / 热键 / SendInput |
| `github.com/google/uuid` | v1.6.0 | 条目 ID |

---

## 3. 技术选型

**Go + Wails v2**:Win32 底层全部用 Go 手写,UI 交给 WebView2 里的 HTML/CSS。

选型理由:

- 剪贴板监听、全局热键、焦点还原、按键注入全是 Win32 syscall,Go 直接可做
- Paste 的观感(毛玻璃、圆角卡片、滑入动效)只有 Web 技术能低成本复刻;纯 Go GUI 库(fyne/walk)做不出来
- 产物为单文件 exe,Win11 自带 WebView2,无运行时依赖

被否方案:

| 方案 | 否决原因 |
|---|---|
| Electron + Node | 体积 200MB+;剪贴板只能轮询,回贴注入仍需原生模块 |
| 纯 Go + fyne | 无亚克力/无平滑动画,观感差距明显 |
| C# WPF | 本机无 .NET SDK |

---

## 4. 架构设计

### 4.1 目录结构

```
paste-win/
├── DESIGN.md                  # 本文档
├── main.go                    # 入口:Wails 启动 + 托盘 + 生命周期编排
├── app.go                     # App struct,Wails 绑定方法(前端 API 面)
├── go.mod / go.sum
├── wails.json                 # Wails 构建配置
├── internal/
│   ├── winapi/                # 最底层,只包 syscall,无业务逻辑
│   │   ├── consts.go          # 消息常量、格式常量、VK 码
│   │   ├── msgwin.go          # 隐藏消息窗口:类注册 + 独立消息循环
│   │   ├── clipboard.go       # Open/Get/Set/EnumClipboardFormats
│   │   ├── formats.go         # CF_UNICODETEXT / CF_HDROP / DIB / HTML / RTF 读取
│   │   ├── dib.go             # CF_DIB(V5) → image.Image → PNG 编解码
│   │   ├── window.go          # GetForegroundWindow / 焦点还原 / AttachThreadInput
│   │   ├── input.go           # SendInput 合成 Ctrl+V
│   │   └── process.go         # GetClipboardOwner → 来源进程名
│   ├── clipwatch/             # 剪贴板监听服务
│   │   └── watcher.go         # WM_CLIPBOARDUPDATE → 去重 → 解析 → 入库
│   ├── hotkey/hotkey.go       # RegisterHotKey + WM_HOTKEY 回调
│   ├── paster/paster.go       # 焦点还原 + 写剪贴板 + SendInput
│   ├── store/                 # 持久化
│   │   ├── store.go           # 打开 + PRAGMA + 迁移
│   │   ├── item.go            # 条目 CRUD / 查询 / 清理
│   │   ├── pinboard.go
│   │   └── settings.go
│   ├── blobs/blobs.go         # 图片落盘 + 缩略图生成
│   ├── exclusion/exclusion.go # 进程黑名单 + 敏感格式标记
│   ├── autostart/autostart.go # HKCU\...\Run 开机自启
│   └── tray/tray.go           # 托盘图标 + 菜单
└── frontend/
    ├── dist/                  # 静态资源,直接内嵌(不引入打包器)
    │   ├── index.html
    │   ├── style.css
    │   └── app.js
    └── wailsjs/               # Wails 生成的 JS 绑定(勿手改)
```

### 4.2 分层

```
        frontend (HTML/CSS/JS)
                ↕  Wails 绑定
        app.go  ── 绑定层,只做参数转换与事件派发
                ↕
  ┌────────┬────────┬────────┬────────┬────────┐
 clipwatch  hotkey  paster   store   blobs   tray
  └────────┴────────┴────────┴────────┴────────┘
                ↕
        internal/winapi  ── 纯 syscall 封装
```

### 4.3 关键机制

#### 4.3.1 隐藏消息窗口(整个应用的事件源)

`internal/winapi/msgwin.go` 注册专用窗口类,创建不可见 HWND,在**独立 goroutine + `runtime.LockOSThread`** 上跑自己的 `GetMessage/DispatchMessage` 循环。

它与 Wails 自己的消息循环互不干扰——每个线程可有独立消息循环。该窗口同时接收:

- `WM_CLIPBOARDUPDATE (0x031D)` → 触发剪贴板读取
- `WM_HOTKEY (0x0312)` → 触发面板显隐

**为何不用轮询**:`GetClipboardSequenceNumber` 轮询有延迟且会丢快速连续复制。事件驱动零延迟、不丢事件。

#### 4.3.2 剪贴板捕获

`AddClipboardFormatListener(hwnd)` 注册。收到事件后:

1. `GetClipboardSequenceNumber()` 去重;自写剪贴板时置 `ignoreNext` 标志防回环
2. `OpenClipboard` **带退避重试**(剪贴板常被其他进程占用,5 次 × 20ms)
3. `EnumClipboardFormats` 按优先级取:
   `CF_HDROP > CF_DIBV5/CF_DIB > HTML Format > Rich Text Format > CF_UNICODETEXT`
4. 来源进程:`GetClipboardOwner` → `GetWindowThreadProcessId` → `QueryFullProcessImageName`
5. 敏感内容跳过:检查 `ExcludeClipboardContentFromMonitorProcessing` /
   `CanIncludeInClipboardHistory` 注册格式,值为 0 则不记录(密码管理器通行约定)

#### 4.3.3 回贴(最易踩坑的一步)

唤起面板时记录 `prevHwnd = GetForegroundWindow()`。回车时:

1. 隐藏 Wails 窗口
2. 把选中条目写回剪贴板(设 `ignoreNext`);纯文本模式只写 `CF_UNICODETEXT`
3. 等 ~50ms 让窗口真正隐藏,再还原焦点:
   `SetForegroundWindow(prevHwnd)`;失败则用
   `AttachThreadInput(ourTid, targetTid, true)` → `SetForegroundWindow` → `AttachThreadInput(..., false)`
   变通(Windows 有前台锁定限制)
4. `SendInput` 合成 `Ctrl↓ / V↓ / V↑ / Ctrl↑`

#### 4.3.4 去重与容量控制

连续相同 hash 不重复入库,只把 `created_at` 提到最前。
保留最近 2000 条,`pinned=1` 或属于 pinboard 的不清理,清理时同步删 blob。

---

## 5. 数据设计

数据目录:`%LOCALAPPDATA%\PasteWin\`(`pastewin.db` + `blobs\`)

```sql
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE items (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL,      -- text | image | files | richtext
  content      TEXT NOT NULL DEFAULT '',  -- 主载荷:文本正文 / HTML 源 / 文件路径 JSON
  plain_text   TEXT NOT NULL DEFAULT '',  -- 纯文本表示,供搜索与「纯文本粘贴」
  preview      TEXT NOT NULL DEFAULT '',  -- 卡片摘要
  blob_path    TEXT NOT NULL DEFAULT '',  -- 图片 PNG 相对路径
  thumb_path   TEXT NOT NULL DEFAULT '',
  source_app   TEXT NOT NULL DEFAULT '',  -- 来源进程名
  source_title TEXT NOT NULL DEFAULT '',
  hash         TEXT NOT NULL,      -- 内容去重
  pinned       INTEGER NOT NULL DEFAULT 0,
  pinboard_id  TEXT REFERENCES pinboards(id) ON DELETE SET NULL,
  created_at   INTEGER NOT NULL,   -- Unix 毫秒
  seq          INTEGER NOT NULL DEFAULT 0,  -- 历史顺序(单调递增),见下
  byte_size    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_items_created  ON items(created_at DESC);
CREATE INDEX idx_items_seq      ON items(seq DESC);
CREATE INDEX idx_items_hash     ON items(hash);
CREATE INDEX idx_items_pinned   ON items(pinned DESC, seq DESC);
CREATE INDEX idx_items_pinboard ON items(pinboard_id, seq DESC);

CREATE TABLE pinboards (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  sort_order INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

### 5.1 为什么需要 seq 列

`created_at` 只表达"何时复制",不能表达"在历史里排第几"。两个问题:

- 同一毫秒内连续复制多次时,时间戳相同,排序退化为任意
- 重复复制一条旧内容需要把它"提升"到最前,但提升后的时间戳可能仍与邻居相同

因此引入单调递增的 `seq` 专管顺序,`created_at` 回归纯粹的展示与按日期过滤。
插入与提升都取 `MAX(seq)+1`,保证严格递增。

---

## 6. 接口设计

前端调 `window.go.main.App.Xxx()`;Go 推事件用 `runtime.EventsEmit(ctx, "clip:new", item)` 让面板实时更新。

| 方法 | 说明 |
|---|---|
| `ListItems(filter Filter) []ItemView` | 关键字 / kind / pinboard / 日期范围 |
| `GetItem(id) ItemView` | |
| `DeleteItem(id)` / `TogglePin(id)` | |
| `UpdateItemContent(id, text)` | 就地编辑 |
| `PasteItem(id, plainText bool)` | 回贴 |
| `PasteStack(ids []string)` | 顺序粘贴多条 |
| `ListPinboards()` / `CreatePinboard(name)` | |
| `RenamePinboard` / `DeletePinboard` | |
| `MoveToPinboard(itemId, pinboardId)` | |
| `GetSettings()` / `UpdateSettings(s)` / `SetHotkey(accel)` | |
| `PauseCapture(bool)` / `HidePanel()` / `Quit()` | |

---

## 7. 实施状态

| # | 步骤 | 状态 | 备注 |
|---|---|---|---|
| 1 | 工具链打通 | ✅ | 无 MSVC 也能构建,风险已解除 |
| 2 | 隐藏消息窗口 + 剪贴板监听 | ✅ | 事件驱动,文本/图片/文件/富文本均可捕获 |
| 3 | SQLite 建表 + 入库 + 去重 | ✅ | 含 seq 排序修正 |
| 4 | 托盘 + `Ctrl+Shift+V` 唤起 | ✅ | 托盘自实现,未引入第三方库 |
| 5 | 卡片 UI + 搜索 + 键盘导航 | ✅ | 原生 JS,无打包器 |
| 6 | 回贴:焦点还原 + SendInput | ✅ | 见 8.1,过程中修掉两个关键缺陷 |
| 7 | 图片 / 文件 / 富文本 | ✅ | 图片落盘 + 缩略图;DIB 编解码有测试 |
| 8 | Pinboard / Quick Paste / 纯文本粘贴 / 编辑 | ✅ | |
| 9 | 排除规则 + 开机自启 + 设置界面 | ✅ | |
| 10 | 打包单 exe | ✅ | 16 MB |

### 7.1 与计划的偏差

**Paste Stack 实现为「顺序粘贴」而非「交互式粘贴栈」。**

Paste 原版的粘贴栈是:选中多条后,每次按 `Cmd+V` 自动粘出下一条。
要做到这一点,必须在粘贴栈激活期间用低级键盘钩子(`WH_KEYBOARD_LL`)拦截 `Ctrl+V`,
并借助 `KBDLLHOOKSTRUCT.flags` 的 `LLKHF_INJECTED` 位区分"用户按的"与"自己注入的",
否则会陷入自触发循环。

当前实现是"一次性按顺序逐条粘贴",不做按键拦截。功能上覆盖了"把多条内容依次填进表单"
这个主要用途,但交互与 Paste 不同。这是有意取舍,不是遗漏。

其余功能均已实现。

---

## 8. 实施中发现的缺陷与修复

以下四个问题都是**实测才发现**的,静态审查不易察觉。

### 8.1 `INPUT` 结构体少了 8 字节,导致所有按键注入失败(严重)

`SendInput` 的 `INPUT` 结构里是一个 union。union 的大小由**最大**成员决定 ——
是 `MOUSEINPUT` 的 32 字节,不是 `KEYBDINPUT` 的 24 字节。
因此 `sizeof(INPUT)` = 8 + 32 = **40**,不是按 `KEYBDINPUT` 算出来的 32。

少补这 8 字节会让 `SendInput` 直接返回 `ERROR_INVALID_PARAMETER`,
且**所有**按键注入全部失效 —— 包括核心的回贴功能。
失败是静默的(没有崩溃、没有日志),只表现为"按了回车没反应"。

已在 `winapi/input.go` 显式补齐,并用 `TestInputStructLayout` 钉死结构大小防回归。

### 8.2 `AttachThreadInput` 未锁线程,会永久破坏全局热键(严重)

`ForceForeground` 用 `AttachThreadInput` 绕过 Windows 前台锁定。
该 API 的 attach / detach 以"当前线程"为一方且必须成对。

Go 的 goroutine 会在函数执行途中被调度到别的 OS 线程上。一旦发生,
`defer` 里的 detach 就作用在**错误的线程**上,attach 永远撤不掉 ——
输入队列被永久挂在一起,表现为**全局热键从此失效**,直到进程退出才恢复。

这个 bug 由测试靶子暴露:靶子进程一运行,热键就失灵;杀掉后立刻恢复。
修复是在 `ForceForeground` 与 `SetFocusCrossThread` 开头 `runtime.LockOSThread()`。

### 8.3 还原窗口时控件焦点不回来,粘贴静默失效(严重)

真实的用户场景是:用户正在某个输入框里打字 → 面板抢走激活 → 回贴时还原窗口。

实测确认:**Windows 不会自动把焦点还给原来的输入框**(见 `tools/focustest` 第二阶段)。
只调 `SetForegroundWindow` 的话,`Ctrl+V` 会落到顶层窗口上,粘贴静默失败。

修复:唤起面板前用 `GetGUIThreadInfo` 记录"哪个控件持有焦点"
(见 `winapi.CaptureFocusTarget`),回贴时用 `SetFocusCrossThread` 还原 ——
后者同样需要 `AttachThreadInput`,因为 `SetFocus` 只能作用于调用线程的窗口。

### 8.4 按时间戳排序不确定

`created_at` 秒级精度时,同一秒内的多条记录排序退化为任意。
改为毫秒后仍有"同一毫秒"的边界情况,故引入单调递增的 `seq` 列专管顺序(见 5.1)。

### 8.5 `OnBeforeClose` 钩子把程序化退出也挡死了(严重)

为了让"点窗口关闭按钮"只收起面板而不退出,`OnBeforeClose` 无条件返回 `true`。
但 Wails 的 `Frontend.Quit()` 实现是:

```go
func (f *Frontend) Quit() {
    if f.frontendOptions.OnBeforeClose != nil && f.frontendOptions.OnBeforeClose(f.ctx) {
        return   // ← 直接放弃退出
    }
    f.mainWindow.Invoke(winc.Exit)
}
```

也就是说这个钩子同时挡住了**所有**程序化退出 —— 托盘菜单里的「退出」点了没有任何反应。
修复是用一个 `quitting` 标志区分"用户点了关闭按钮"与"我们确实要退出"。

### 8.6 缺少单实例保护(严重)

原本没有做单实例控制,每启动一次就多一个进程:各自的托盘图标、各自的剪贴板监听,
而且只有第一个能注册上全局热键,其余只会在日志里报错。
用户看到的是托盘里堆了一排图标,且退出其中一个对其余毫无影响。

修复用命名互斥体(`Local\PasteWin.mutex`)+ 命名事件:
第二个实例不再启动,而是置位事件唤醒已有实例把面板弹出来。

顺带加了 `--quit` 开关(同样基于命名事件),供脚本与卸载流程优雅关闭,
避免用 `taskkill` 硬杀而跳过清理。

### 8.7 退出路径的兜底

Wails 的 `runtime.Quit` 依赖"给主线程投递退出消息"这条路径,
实测在本机出现过返回了但主消息循环未终止的情况(进程继续存活)。

与其依赖这条不受控的路径,`App.Quit` 改为:先尝试优雅退出,
宽限期(`quitGracePeriod`,1.5 秒)内没退干净就自行清理并 `os.Exit(0)`。
清理逻辑(`App.Cleanup`)用 `sync.Once` 保护,优雅路径与强制路径共用一份,
不会重复关闭数据库或重复注销热键。

### 8.8 CSS 的 `display` 覆盖了 `hidden` 属性,弹层关不掉(严重)

`#editor` / `#settings` / `#empty` 在样式表里写了 `display: flex`,
而 HTML 的 `hidden` 属性是靠**浏览器默认样式表**里的 `display: none` 生效的 ——
作者样式表的优先级高于默认样式表,于是 `hidden` 完全失效。

后果:设置弹层与编辑弹层**从页面加载起就一直显示**。
它们是 `position: fixed; inset: 0` 的全屏遮罩,把整个面板盖住,
表现为"按了热键只看到一个设置框,看不到任何剪贴板内容",且卡片点不动。

修复:全局加一条 `[hidden] { display: none !important; }`。

> 这类问题的教训:凡是用 `hidden` 属性切换显隐的元素,样式表里都不要直接写
> `display`,或者必须配一条 `[hidden]` 兜底规则。

### 8.9 方向键被搜索框吞掉、滚轮完全无效(严重)

两个独立的前端错误,共同造成"面板没法用键盘和滚轮操作":

- **方向键**:搜索框在面板弹出时会自动获得焦点(刻意的:弹出即可直接输入)。
  但键盘处理里有一道守卫 `if (isTyping(e.target) && e.key !== 'Enter') return;`,
  把方向键一并让给了输入框 —— 于是左右浏览**永远不会生效**。
  改为只放行普通字符,方向键 / Enter / Esc / Ctrl 组合键始终由面板处理。

- **滚轮**:卡片轨道是 `overflow-x: auto` 的横向容器,
  而浏览器**不会**把纵向滚轮作用到横向容器上。必须自己把 `deltaY` 映射到 `scrollLeft`。

顺带把 `#track` 上的 `scroll-behavior: smooth` 去掉:它会让滚轮滚动变成动画,手感发飘;
平滑只在键盘导航时由 `scrollIntoView` 显式指定。

### 8.10 `ForceForeground` 把自己的消息线程也挂进了输入队列(严重)

原实现用 `AttachThreadInput` 把本线程同时挂到**目标窗口线程**和**当前前台线程**上。
但当目标是 PasteWin 自己的窗口时(唤起面板要抢焦点),
"目标窗口线程"就是本进程的消息循环线程 —— 把自己的消息线程也拉进输入队列共享,
反而让 `SetForegroundWindow` 失败。

后果:面板显示了却**没有拿到键盘焦点**,用户按方向键、Esc 全部落到别的窗口上,
表现为"面板只能看、不能用键盘操作,只能靠鼠标点击粘贴"。

修复为两级尝试,只在必要时才动输入队列,且**只挂对方、不挂自己**:

1. 直接 `SetForegroundWindow`(本进程持有热键,通常直接放行)
2. 挂到当前前台线程后再试

同时补了"面板失去焦点自动收起":点桌面、点别的窗口都会关掉面板。
这正是这类启动器面板的通行行为,原先缺失导致用户觉得"只能靠粘贴才能关掉它"。

### 8.11 日志文件永远是空的:`io.MultiWriter` 在第一个 writer 上短路(严重)

排查回贴失效时最要命的问题:**装好的应用写不出任何日志**。

原因是 `log.SetOutput(io.MultiWriter(os.Stderr, f))`。
`io.MultiWriter` 的语义是"逐个写,遇到第一个错误就**立即返回**" ——
它不会跳过失败的 writer 继续写后面的。

托盘应用以无控制台的方式启动(开机自启、快捷方式、`start`)时,
`os.Stderr` 是一个无效句柄,每次写入都失败,于是**排在后面的文件一个字节都拿不到**。
从外面看就是"日志文件存在但一直是 0 字节",而这类应用又没有控制台,
等于完全没有可观测性。

修复是给每个 writer 套一层吞掉错误的包装(`tolerantWriter`),
让某个输出端的失败不影响其它输出端。

> 教训:凡是"同时写多个目的地"的地方,都要先问一句
> **其中一个坏了会不会把其余的也带走**。`io.MultiWriter` 的答案是不会继续写。

### 8.12 排障时的最大干扰:会话锁屏

回贴相关的现象一度看起来自相矛盾:

- 同一个 `focustest`,十分钟前通过,十分钟后失败
- `GetForegroundWindow()` 返回 0,`GetCursorPos()` 报 `Access is denied`
- 日志里目标窗口是 `0()`,面板怎么都抢不到前台

根因与代码无关:**当时会话处于锁屏状态**(`LogonUI.exe` 在运行)。
锁屏时输入桌面是 `Winlogon`,普通进程既拿不到输入桌面,
也读不到前台窗口 —— 于是所有涉及前台/注入的操作全部失效。

这类干扰很容易被误判成程序缺陷,所以加了 `tools/deskprobe`:
它直接打印当前线程所在的窗口站、桌面名,以及能否打开输入桌面。
**做任何交互验证之前先跑一遍**,能省掉大量无效排查。

```bash
./build/deskprobe.exe
# 正常:  OpenInputDesktop 成功,结论"可以访问输入桌面"
# 锁屏:  Access is denied,结论"按键注入与抢前台都不会生效"
```

### 8.13 回贴粘不出去:把异步的前台切换当成了同步结果(严重)

这是"双击卡片/按回车不粘贴,还得手动再按一次 Ctrl+V"的**真正根因**。

`ForceForeground` 原本这样判断成败:

```go
procSetForegroundWindow.Call(hwnd)
if GetForegroundWindow() == hwnd { return nil }   // ← 读一次就下结论
```

**但前台切换是异步的。** `SetForegroundWindow` 返回时,系统只是把激活请求
交给了目标线程,真正切换完成要等它处理。这中间存在一个窗口期:
原前台窗口已取消激活、新前台尚未就位,此时 `GetForegroundWindow()` 返回 **0**。

日志里看得最清楚 —— 返回值和紧随其后的读数互相矛盾:

```
抢前台: 直接 SetForegroundWindow ret=1 → 前台=0     ← 调用成功,读数却是空
```

于是判定为"失败",整个回贴被放弃:剪贴板里内容是对的,按键一个都没发出去。
用户看到的就是"没反应,得自己再按一次 Ctrl+V"。

修复是把"读一次"改成"轮询等待"(上限 150ms):

```go
func waitForeground(hwnd uintptr) bool { /* 轮询到 hwnd 成为前台,或超时 */ }
```

> 教训:`SetForegroundWindow` 的返回值和 `GetForegroundWindow()` 都不是
> "切换是否完成"的可靠信号 —— 前者只说明请求被受理,后者受异步时机影响。
> 涉及跨进程激活这类异步状态,只能等,不能问一次就下结论。

### 8.14 面板抢焦点:对尚未显示的窗口抢前台必然失败(严重)

`ShowPanel` 里 `runtime.WindowShow` 之后紧接着调 `focusPanel()`。
但 `WindowShow` 只是把"显示"投递到主线程执行,**返回时窗口往往还是隐藏状态**,
而对一个隐藏窗口调 `SetForegroundWindow` 必然失败。

表现是"面板时好时坏地拿不到键盘焦点" —— 全看那几十毫秒里主线程有没有跑完。
日志里 `SetForegroundWindow ret=0` 与 `ret=1` 交替出现,就是这个原因。

修复:先轮询等 `IsWindowVisible` 为真,再抢前台。

### 8.15 交还焦点的顺序:先交还,再收面板

原顺序是"先藏面板,再交还焦点"。改成了**先交还焦点、再收起面板**。

理由是权限:面板还在前台时,本进程就是"前台进程",`SetForegroundWindow`
才会被系统真正执行;一旦先把面板藏起来,前台窗口就变成空,
此时我们对前台的处置权也一并失去。

这一条与 8.13 是同一次故障的两个侧面 —— 单独改顺序并不能解决问题
(实测改完顺序后仍然失败,因为判定逻辑本身是错的),但顺序本身是对的,
留着更稳。

---

### 8.16 面板在多屏 + 混合缩放下落到错误的显示器上(严重)

**现象**:三块屏(主屏 1920×1200 @125%,另两块 1920×1080 @100%)。
面板应当贴在光标所在屏幕的底边,实际却会跑到别的屏幕上,或者尺寸不对。

实测(探针把光标依次放到每块屏中央,再给消息窗口投 `WM_HOTKEY`):

| 屏 | 期望 | 实际 |
|---|---|---|
| DISPLAY1 @125% | 1920×280 @0,860 | **2400×350** @0,860 |
| DISPLAY2 @100% | 1920×280 @882,-280 | 1920×280 @882,-280(碰巧对) |
| DISPLAY3 @100% | 1920×280 @-1038,-280 | 1920×280 @**-156,-1360** |

**根因是 Wails v2.16.0 的两个坐标系陷阱**(`winc/controlbase.go`),
`runtime.WindowSetSize` / `WindowSetPosition` 在多屏下都不能用来做绝对定位:

```go
func (cba *ControlBase) SetSize(width, height int) {
	width, height = cba.scaleWithWindowDPI(width, height)  // 又乘一遍当前显示器 DPI
	w32.MoveWindow(cba.hwnd, x, y, width, height, true)
}

func (cba *ControlBase) SetPos(x, y int) {
	info := getMonitorInfo(cba.hwnd)                       // 窗口"当前所在"显示器
	w32.SetWindowPos(cba.hwnd, w32.HWND_TOP,
		int(info.RcWork.Left)+x, int(info.RcWork.Top)+y,   // 当成相对偏移,不是绝对坐标
		0, 0, w32.SWP_NOSIZE)
}
```

- `SetSize` 把尺寸乘上**窗口当前所在显示器**的缩放,所以主屏 125% 下 1920 变成 2400。
- `SetPos` 的参数是**相对窗口当前所在显示器工作区左上角**的偏移量,不是屏幕绝对坐标。
  DISPLAY3 那次窗口还在 DISPLAY2 上,于是 `882+(-1038), -1080+(-280)` = `-156,-1360`。
  DISPLAY2 那次碰巧对,只是因为窗口当时在原点为 (0,0) 的主屏上。

**修复**:不再使用 Wails 的这两个方法,自己调 `SetWindowPos`
(`winapi.SetWindowBounds`,物理像素、绝对坐标)。本进程在清单里声明了
`permonitorv2`,Win32 不做任何 DPI 虚拟化,坐标就是屏幕真实像素。

尺寸上做了一处有意的区分:**宽度**直接用工作区物理宽度铺满(缩放会让面板越出屏幕边缘);
**高度**按逻辑像素乘该屏缩放换算,这样面板在 100% 和 125% 的屏幕上看起来一样高。

#### 8.16.1 附带发现:Wails 会照单全收 `WM_DPICHANGED` 的建议矩形

面板从 125% 的屏挪到 100% 的屏时,Windows 补一条 `WM_DPICHANGED`,
而 Wails 直接套用它建议的矩形(`windows/window.go` 的 `0x02E0` 分支)。
建议值是按新旧 DPI 之比缩放出来的:`1920×280` 会变成 `1536×224`。

这条消息是异步到的,`SetWindowPos` 返回时还看不出来。因此定位之后
起一个 goroutine 回读校验、不符就重设(`settlePanelBounds`),最多 8 轮 × 25ms。

#### 8.16.2 附带发现:无条件回读会把瞬时错误固化成用户偏好

一开始的做法是"收起面板时回读实际高度并落库",用来兜住用户直接拖
无边框窗口边框那条路径。但窗口尺寸在跨屏瞬间可能正被 `WM_DPICHANGED`
改坏,于是坏的 224 被当成用户偏好存了下来 —— 之后每次唤起都按 224 渲染,
成了"垃圾进垃圾出"。

改成**只在矩形确实和 `ShowPanel` 设下去的值不同时才保存**,也就是只在
用户真的改过尺寸时才落库。

#### 8.16.3 顺带修掉:面板高度不持久

`ShowPanel` 每次都按 `settings.PanelHeight` 重设尺寸,而用户拖边框改出来的
高度从来没有被写回去,所以"调了等于没调"。现在有两条路径都会落库:
拖拽把手(`StartPanelResize`)和直接拖边框(收起时的回读兜底)。

落库的是**逻辑像素**,这样换到别的缩放比例的显示器上观感一致。

### 8.17 面板调整大小的入口不可见

面板是无边框窗口,虽然还带着 `WS_THICKFRAME`(能拖边框改大小),
但没有任何视觉提示,用户不知道它可以调整。

在面板顶边正中加了一个可见的拖拽把手(`#resize-grip`):一条 56×4 的
圆角横条,悬停时变亮变宽。高度刻意压到 12px —— 顶栏的内边距正好是 12px,
把手只盖住这段留白,不会挡住下面的 Pinboard 标签。

拖拽本身在 Go 侧完成(`StartPanelResize` 轮询鼠标左键状态直到松手),
而不是每次 `mousemove` 都过一次 JS↔Go 桥。面板贴屏幕底边,所以高度
就是"工作区底边 - 光标 Y",光标甩出工作区时按边界钳住即可。

把手所在区域要显式声明 `--wails-draggable: no-drag` —— 顶栏整条是窗口
拖拽区,不排除的话按下去会变成拖窗口。

---

## 9. 构建

```bash
./build.sh          # 常规构建
./build.sh --full   # 含 Wails 绑定生成(很慢,见下)
```

`build.sh` 封装了本机环境的四个坑:

1. Go 不在 `PATH`,在 `D:\code-env\.tools\go\bin`
2. `proxy.golang.org` 不通,必须 `GOPROXY=https://goproxy.cn,direct`
3. `CGO_ENABLED=0`,故 SQLite 必须用纯 Go 驱动
4. **`wails build` 的绑定生成步骤在本项目上要跑 11 分钟以上**

第 4 点值得说明:Wails 生成 JS 绑定时用 `go/packages` 加载整个依赖图,
而 `modernc.org/sqlite` 是 C 转译产物、包数极多,拖垮了这一步。
本项目前端直接调用运行时注入的 `window.go.main.App.Xxx()`,
不依赖生成的 `frontend/wailsjs`,因此构建默认加 `-skipbindings`,耗时回到 **4 秒**。

---

## 10. 验证

### 10.1 自动化测试

```bash
go test ./internal/...
```

覆盖:
- `winapi`:剪贴板文本往返(含中文/emoji)、剪贴板序号递增、DIB 编解码往返、
  垃圾输入拒绝、`INPUT` 结构体大小断言
- `store`:插入/去重提升、关键字与类型过滤、LIKE 通配符转义、
  置顶排序、清理边界(受保护条目不被删)、分组 CRUD、配置往返与非法值修正

### 10.2 焦点与注入链路自测

```bash
./build/focustest.exe
```

在**单进程内**建两个窗口,人为制造"目标窗口不是前台"的初始条件,
再走产品的 `ForceForeground` + `SendCtrlV`,最后校验编辑框内容。

单进程是刻意的:外部脚本(bash / powershell)每次调用都会创建自己的窗口并抢走前台,
导致被测窗口的前台状态不可控、结果随机。这正是 8.2 与 8.3 被发现的方式。

### 10.3 手工验收

`tools/sendkeys`(注入按键 / 写剪贴板)与 `tools/keytarget`(带编辑框的靶子窗口)
配合可复现完整链路,且**不依赖 PowerShell** —— 后者的控制台窗口会抢走前台干扰测试。

```bash
./build/sendkeys.exe -setclip "测试内容"        # 写剪贴板
./build/sendkeys.exe ctrl+shift+v              # 唤起面板
./build/sendkeys.exe enter                     # 回贴
```

**做回贴测试必须用无控制台的 `*w.exe` 版本**:

```bash
./build/sendkeysw.exe ctrl+shift+v
./build/keytargetw.exe -out result.txt -hold 30s
```

控制台程序一运行就把焦点抢到终端,面板会立刻"失去焦点自动收起",
测试根本走不到注入那一步 —— 这一条曾让我误判了好几轮。

`keytarget` 的两个开关都是为了对付这种干扰:

| 开关 | 作用 |
|---|---|
| `-hold <时长>` | 持续把前台抢回来,否则终端会不断把焦点抢走,面板捕获到的目标根本不是靶子 |
| (内建) | 当前台是 PasteWin 面板(`wailsWindow` 类)时**主动让位**,否则会把面板顶掉 |

靶子的 WndProc 也处理了 `WM_SETFOCUS` 把焦点还给编辑框 —— 真实程序都会这么做,
裸的 Win32 窗口不会。不补这一步,靶子就无法区分"产品没还原控件焦点"
与"靶子自己不还原焦点"。

需要人工确认的项目:托盘图标显示与菜单、亚克力透明效果、开机自启生效、
密码管理器复制不被记录、面板视觉效果。

### 10.4 日志

应用把日志写到 `%LOCALAPPDATA%\PasteWin\pastewin.log`(超过 2 MB 轮转一代)。

托盘应用没有控制台,这是唯一的可观测手段。回贴这条链路依赖时序,
日志里按顺序记下了每一步:

```
回贴开始: 条目=… 目标窗口=…(类名) 线程=… 控件=…(类名) 当前前台=…
回贴: 剪贴板已就绪
抢前台: 目标=…(类名) 线程=… | 当前前台=…(类名) 线程=… | 本线程=…
抢前台: 直接 SetForegroundWindow ret=… err=… → 前台=…
抢前台: AttachThreadInput(本线程=…, 前台线程=…) ret=…
抢前台: 挂载后 SetForegroundWindow → 前台=…
回贴: 焦点已交还,当前前台=…(类名)
回贴: 注入前 前台=…(类名) 焦点控件=…(类名)
回贴: Ctrl+V 已注入,注入后前台=…
```

"注入前"那一行是定位回贴失效的关键:它回答的是
**注入的那一刻,按键到底会送到哪个窗口、焦点在哪个控件上**。
窗口类名能进一步区分目标是不是自绘界面(`Chrome_WidgetWin_*` 一类),
那种程序的输入框不是真正的子窗口,跨进程 `SetFocus` 对它没有意义。

`抢前台` 那几行来自 `winapi.ForceForeground`,是 8.13 / 8.14 两次故障的
直接证据来源 —— 没有这几行,`ret=1 → 前台=0` 这种自相矛盾的组合
根本无从发现。

---

## 11. 搜索增强与 OCR

### 11.1 先纠正一个此前的判断

评审时我说过"搜索没有分词,搜『循环检测』匹配不到"。**这是错的。**

现有实现是 `plain_text LIKE '%关键字%'`,而 `LIKE` 是**子串匹配** ——
"循环检测"本身就是 `消息循环检测_1792034852` 的连续子串,能直接命中。
中文子串搜索一直是好的,不需要分词。

真正缺的只有两件事:

- **拼音**:打 `xiaoxi` 或首字母 `xx` 找不到「消息循环」
- **相关性排序**:结果纯按时间倒序,搜出来的第一条不一定最相关

### 11.2 方案:预计算 `search_text` 列,而不是上 FTS5

最初建议换 FTS5。看过实际约束后改主意了,理由:

- FTS5 的 `unicode61` 分词器把一整串中文当成**一个** token,
  要让它能搜中文反而得先自己做 CJK 预处理 —— 也就是说 **FTS5 并不能省掉这件事**,
  它只是把预处理好的文本装进一个更复杂的容器
- FTS5 需要影子表 + 触发器同步,而**影子表与主表不同步是"搜索坏了"这类
  bug 的经典来源**,排查起来很隐蔽
- 条目上限 2000,`LIKE '%…%'` 全表扫描是毫秒级,性能根本不是瓶颈

所以走更简单的路:加一列预计算的 `search_text`,查询时多一个 LIKE 条件。

**关键洞察:`LIKE` 是子串匹配,所以拼音根本不需要按音节切分。**
把一段中文的完整拼音**连起来**存进去,任何子串都能命中:

```
原文        消息循环检测_1792034852
全拼(连写)   xiaoxixunhuanjiance_1792034852
首字母       xxhxjc
```

于是:

| 用户输入 | 匹配方式 |
|---|---|
| `循环检测` | 原文子串 ✓ |
| `xunhuan` | 命中 `…xiaoxixunhuanjiance…` 的子串 ✓ |
| `xxhx` | 命中 `xxhxjc` 的子串 ✓ |
| `sk-043b` | 原文子串 ✓ |

**查询侧不需要任何转换** —— 用户输入 lower 一下直接丢进 LIKE 即可。
这是选它而不是选 FTS5 最大的好处:没有查询解析器,没有查询语法要维护。

已知取舍:

- 多音字只取字典里的**首选读音**(如「重庆」记 `zhongqing`),搜 `chongqing`
  命中不到。要提升召回可以开 `Heteronym` 索引全部读音,代价是索引膨胀一倍,暂不做
- 首字母连写会带来一定误命中(任意文本只要首字母序列吻合就会命中)。
  但它是与其它条件 OR 的,不会把正确结果挤掉,只影响排序

### 11.3 OCR:为什么要绕道 PowerShell

目标:截图里的文字也能被搜到。Paste 有,`Win+V` 完全没有。

Windows 自带 OCR(`Windows.Media.Ocr`),但它是 **WinRT API**:

- 从 Go 直接调需要手写 COM/WinRT 的 vtable 互操作,不现实
- Tesseract 一类需要 cgo,与本项目 `CGO_ENABLED=0` 的约束冲突
- 本机没有 .NET SDK,写不了 C# 辅助进程

PowerShell 5.1 **自带 WinRT 投影**,是这台机器上唯一现成的路子。
所以 `internal/ocr/ocr.ps1` 负责调用引擎,Go 侧负责起进程、收结果。

实测:一张 340KB 的截图约 **2 秒**(其中约 0.8 秒是 PowerShell 启动开销),
识别语言 `zh-Hans-CN` 可用。对后台索引来说完全够用。

**脚本文件必须是纯 ASCII。** PowerShell 5.1 在文件不带 UTF-8 BOM 时按
**系统 ANSI 代码页**读取 `.ps1`,本机是 GBK —— 中文注释会被解码坏掉,
进而把语法解析也带崩(实测报出的行号和实际内容对不上,极难排查)。
中文说明一律放在 Go 侧,脚本里只留英文注释。

**另一条实测结论**:`StorageFile.GetFileFromPathAsync` 要求**反斜杠**路径,
传正斜杠会抛 `发生一个或多个错误`,而错误信息完全不指向真正的原因。

### 11.4 OCR 什么时候跑

不在捕获路径上同步跑 —— 那会让复制动作卡 2 秒。改为:

1. 图片条目入库后投入一个**单消费者的后台队列**
2. 工作协程逐张调用 OCR(串行:PowerShell 进程本身很重)
3. 成功则写回 `ocr_text` 与 `search_text`,并通知前端刷新
4. 条目在识别完成前被删除 → 跳过;识别中进程退出 → 放弃该条,下次启动重来

图片落盘用的是 PNG,而 WinRT OCR 直接接受 PNG 文件,不需要额外解码。

### 11.5 表结构变更

`user_version` 1 → 2,新增三列(已有库走 `ALTER TABLE` + 回填):

```sql
ALTER TABLE items ADD COLUMN ocr_text    TEXT    NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN ocr_done    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN search_text TEXT    NOT NULL DEFAULT '';
```

- `ocr_text`:OCR 识别结果,仅图片条目非空
- `ocr_done`:是否**尝试过**识别(0 = 还没轮到)
- `search_text`:由 `plain_text + ocr_text + source_app` 预计算,供 LIKE 直接匹配

**`ocr_done` 为什么要单独一列。** 识别不出文字的图片,`ocr_text` 同样是空。
只看 `ocr_text` 会把"识别过但图里没字"误判成"还没识别",于是**每次启动
都重新跑一遍** —— 每张约 1.5 秒的 PowerShell 进程,几十张就是可感知的开销。
"识别过"和"识别出了内容"是两件事,不能用同一个字段兼表。

回填在迁移时同步完成,与建表同事务 —— 半途失败整体回滚,不会留下
"列加好了但检索文本是空的"这种中间状态。历史图片的 OCR 由后台队列
在启动后补做,不需要用户操作。

### 11.6 识别前先放大:实测有效,但治不了全部

上线后第一个真实反馈是:**搜不到截图里的人名**。查库发现引擎把
「陈」认成了「阝东」、「诗」认成了「讠寺」—— 不是搜索坏了,是识别错了。

根因是字太小。Windows OCR 对字高约 40px 的文本最准,而聊天窗口截图
这类小字远低于此,引擎会把汉字按**部件**切分。于是加了预处理:
识别前把图片放大到长边约 2500px(只放大不缩小,上限 4 倍)。

同一张图的对照:

| | 识别结果 |
|---|---|
| 原图 | **阝东讠寺** ä … 分一篇**文世** … 每次版**不**更新 … do**（**的链接 … 才能**染**展示 … 这样**朩地** |
| 4 倍 | **陈**讠寺 ü … 分一篇**文档** … 每次版**本**更新 … **doc**的链接 … 才能**渲染**展示 … 这样**本地** |

正文几乎全对,「陈」也回来了。

**但「诗」「颖」仍然是错的** —— 汉字被拆成部件是引擎自身的切分行为,
放大只能缓解、不能根治。这类错误没有后处理能可靠地修回去
(要拼回「讠+寺=诗」需要一套字形分解表,而且很容易把本来正确的
相邻两个字误合并)。这是本方案的能力边界,写在这里以免被当成 bug 反复追。

**放大倍数不是越大越好**:实测 8 倍反而不如 4 倍 —— 过度插值把笔画
糊在一起,切分更难。所以上限卡在 4 倍。

### 11.7 流水线版本与重做

识别质量会随预处理改进而变化,所以老图片需要重做。用 `settings` 表里的
`ocr_pipeline_version` 标记:与代码里的 `ocr.PipelineVersion` 不一致时,
把所有图片重置为待识别,并写入新版本号。

用版本标记而不是"每次启动都重做":后者等于每次开机重跑几十个
PowerShell 进程。而**改动预处理时必须记得递增这个常量** ——
不递增,老图片就会永远停在旧流水线的识别质量上,且没有任何报错。
