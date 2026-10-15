/*
 * PasteWin 前端逻辑。
 *
 * 与 Go 的通道:
 *   - 调用 window.go.main.App.Xxx()  (Wails 运行时注入)
 *   - 订阅 window.runtime.EventsOn(...)
 * 两者在浏览器里直接打开时都不存在,因此统一走 bridge 做空实现兜底,
 * 这样改样式时可以直接用浏览器打开调试。
 */

(() => {
  'use strict';

  const Go = window.go?.main?.App ?? null;
  const Runtime = window.runtime ?? null;

  // 浏览器里预览时的兜底,保证页面不报错
  const bridge = new Proxy({}, {
    get: (_, name) => {
      if (Go && typeof Go[name] === 'function') {
        return (...args) => Go[name](...args);
      }
      return async () => { throw new Error(`未连接到后端: ${String(name)}`); };
    },
  });

  const el = (id) => document.getElementById(id);
  const dom = {
    search: el('search'),
    searchClear: el('search-clear'),
    kindFilter: el('kind-filter'),
    appFilter: el('app-filter'),
    pinboardTabs: el('pinboard-tabs'),
    track: el('track'),
    empty: el('empty'),
    emptyTitle: el('empty-title'),
    emptyHint: el('empty-hint'),
    count: el('status-count'),
    toast: el('status-toast'),
    btnPause: el('btn-pause'),
    btnSettings: el('btn-settings'),
    editor: el('editor'),
    editorText: el('editor-text'),
    settings: el('settings'),
  };

  const state = {
    items: [],
    selected: 0,
    pinboards: [],
    activePinboard: '',   // '' = 全部
    settings: null,
    editingId: null,
    query: '',
  };

  // ------------------------------------------------------------ 工具函数

  function formatTime(iso) {
    const t = new Date(iso);
    if (Number.isNaN(t.getTime())) return '';

    const diff = Date.now() - t.getTime();
    if (diff < 60_000) return '刚刚';
    if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
    if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`;

    const pad = (n) => String(n).padStart(2, '0');
    const hm = `${pad(t.getHours())}:${pad(t.getMinutes())}`;
    if (diff < 172_800_000) return `昨天 ${hm}`;
    return `${t.getMonth() + 1}/${t.getDate()} ${hm}`;
  }

  function toast(msg, ms = 2200) {
    dom.toast.textContent = msg;
    dom.toast.hidden = false;
    clearTimeout(toast._t);
    toast._t = setTimeout(() => { dom.toast.hidden = true; }, ms);
  }

  /** 把前端的诊断信息送到 Go 侧日志。面板没有可见控制台,这是唯一的出口。 */
  function clientLog(msg) {
    if (Go && typeof Go.ClientLog === 'function') {
      try { Go.ClientLog(msg); } catch { /* 日志失败不影响功能 */ }
    }
  }

  /** 把后端返回的错误显示出来。Wails 把 Go 的 error 原样带到 reject 里。 */
  function reportError(prefix) {
    return (err) => {
      const msg = typeof err === 'string' ? err : (err?.message ?? String(err));
      toast(`${prefix}: ${msg}`, 3600);
    };
  }

  // ------------------------------------------------------------ 渲染

  function renderPinboardTabs() {
    const tabs = [];

    const all = document.createElement('div');
    all.className = 'tab' + (state.activePinboard === '' ? ' active' : '');
    all.textContent = '全部';
    all.onclick = () => { state.activePinboard = ''; reload(); };
    tabs.push(all);

    for (const b of state.pinboards) {
      const tab = document.createElement('div');
      tab.className = 'tab' + (state.activePinboard === b.id ? ' active' : '');
      tab.title = b.name;

      const name = document.createElement('span');
      name.textContent = b.name;
      tab.appendChild(name);

      if (b.itemCount > 0) {
        const c = document.createElement('span');
        c.className = 'count';
        c.textContent = String(b.itemCount);
        tab.appendChild(c);
      }

      const del = document.createElement('span');
      del.className = 'tab-del';
      del.textContent = '✕';
      del.title = '删除分组(组内条目会保留)';
      del.onclick = async (e) => {
        e.stopPropagation();
        if (!confirm(`删除分组「${b.name}」?组内条目会保留在历史中。`)) return;
        try {
          await bridge.DeletePinboard(b.id);
          if (state.activePinboard === b.id) state.activePinboard = '';
          await loadPinboards();
          await reload();
        } catch (err) { reportError('删除分组失败')(err); }
      };
      tab.appendChild(del);

      tab.onclick = () => { state.activePinboard = b.id; reload(); };
      tabs.push(tab);
    }

    const add = document.createElement('div');
    add.className = 'tab';
    add.textContent = '＋ 新建';
    add.title = '新建分组';
    add.onclick = async () => {
      const name = prompt('分组名称');
      if (!name?.trim()) return;
      try {
        await bridge.CreatePinboard(name.trim());
        await loadPinboards();
        await reload();
      } catch (err) { reportError('新建分组失败')(err); }
    };
    tabs.push(add);

    dom.pinboardTabs.replaceChildren(...tabs);
  }

  function buildCard(item, index) {
    const card = document.createElement('div');
    card.className = 'card' + (index === state.selected ? ' selected' : '');
    card.dataset.id = item.id;
    card.dataset.index = String(index);
    // 逐张错开入场,列表出现时有连续的流动感
    card.style.animationDelay = `${Math.min(index, 14) * 14}ms`;
    card.title = item.plainText || item.preview;

    const head = document.createElement('div');
    head.className = 'card-head';

    const badge = document.createElement('span');
    badge.className = `badge ${item.kind}`;
    badge.textContent = { text: '文本', richtext: '富文本', image: '图片', files: '文件' }[item.kind] ?? item.kind;
    head.appendChild(badge);

    const app = document.createElement('span');
    app.className = 'card-app';
    app.textContent = item.sourceApp || '未知来源';
    head.appendChild(app);

    const time = document.createElement('span');
    time.className = 'card-time';
    time.textContent = formatTime(item.createdAt);
    head.appendChild(time);

    card.appendChild(head);

    // 正文:按类型分别渲染
    if (item.kind === 'image') {
      const box = document.createElement('div');
      box.className = 'card-thumb';
      const img = document.createElement('img');
      img.src = `/thumb/${encodeURIComponent(item.id)}`;
      img.alt = item.preview || '图片';
      img.loading = 'lazy';
      img.onerror = () => { box.textContent = '图片已丢失'; };
      box.appendChild(img);
      card.appendChild(box);

      // 识别出文字时盖在缩略图下方。截图类内容光看缩略图认不出是哪一张,
      // 一行文字比"图片 800 × 600"有用得多。
      if (item.ocrText) {
        const ocr = document.createElement('div');
        ocr.className = 'card-ocr';
        ocr.textContent = item.ocrText.replace(/\s+/g, ' ').slice(0, 80);
        ocr.title = item.ocrText;
        card.appendChild(ocr);
      }
    } else if (item.kind === 'files') {
      const box = document.createElement('div');
      box.className = 'card-body';
      let paths = [];
      try { paths = JSON.parse(item.content); } catch { paths = []; }

      const list = document.createElement('div');
      list.className = 'file-list';
      for (const p of paths.slice(0, 4)) {
        const row = document.createElement('div');
        row.className = 'file-row';
        const name = document.createElement('span');
        name.className = 'name';
        name.textContent = p.split(/[\\/]/).pop();
        row.appendChild(name);
        list.appendChild(row);
      }
      if (paths.length > 4) {
        const more = document.createElement('div');
        more.className = 'file-row more';
        more.textContent = `还有 ${paths.length - 4} 个…`;
        list.appendChild(more);
      }
      box.appendChild(list);
      card.appendChild(box);
    } else {
      const box = document.createElement('div');
      box.className = 'card-body' + (looksLikeCode(item.plainText || item.content) ? ' mono' : '');
      box.textContent = item.preview || item.plainText || '(空)';
      card.appendChild(box);
    }

    if (item.pinned) {
      const pin = document.createElement('span');
      pin.className = 'card-pin';
      pin.textContent = '★';
      card.appendChild(pin);
    }

    // 前 9 条显示序号,对应 Ctrl+1..9
    if (index < 9) {
      const idx = document.createElement('span');
      idx.className = 'card-index';
      idx.textContent = `⌃${index + 1}`;
      card.appendChild(idx);
    }

    const actions = document.createElement('div');
    actions.className = 'card-actions';

    const pinBtn = document.createElement('button');
    pinBtn.textContent = item.pinned ? '☆' : '★';
    pinBtn.title = item.pinned ? '取消置顶' : '置顶';
    pinBtn.onclick = (e) => { e.stopPropagation(); togglePin(item.id); };
    actions.appendChild(pinBtn);

    const editBtn = document.createElement('button');
    editBtn.textContent = '✎';
    editBtn.title = '编辑';
    editBtn.onclick = (e) => { e.stopPropagation(); openEditor(item.id); };
    actions.appendChild(editBtn);

    const delBtn = document.createElement('button');
    delBtn.className = 'del';
    delBtn.textContent = '🗑';
    delBtn.title = '删除';
    delBtn.onclick = (e) => { e.stopPropagation(); removeItem(item.id); };
    actions.appendChild(delBtn);

    card.appendChild(actions);

    card.onclick = () => { select(index); };
    card.ondblclick = () => paste(item.id, false);

    return card;
  }

  /** 粗略判断是否像代码,用于切换等宽字体。 */
  function looksLikeCode(s) {
    if (!s) return false;
    const codeSignals = /[{};]\s*$|\bfunc\b|\bdef\b|\bconst\b|\bimport\b|=>|<\/?[a-z]+>/i;
    return codeSignals.test(s);
  }

  function render() {
    const frag = document.createDocumentFragment();
    state.items.forEach((item, i) => frag.appendChild(buildCard(item, i)));
    dom.track.replaceChildren(frag);

    const n = state.items.length;
    dom.count.textContent = `${n} 条`;
    dom.empty.hidden = n > 0;
    if (n === 0) {
      dom.emptyTitle.textContent = state.query ? '没有匹配的记录' : '还没有记录';
      dom.emptyHint.textContent = state.query ? '换个关键词试试' : '复制任意内容后回到这里';
    }
    dom.searchClear.hidden = !state.query;
  }

  function select(index) {
    if (index < 0 || index >= state.items.length) return;
    state.selected = index;

    const cards = dom.track.children;
    for (let i = 0; i < cards.length; i++) {
      cards[i].classList.toggle('selected', i === index);
    }
    cards[index]?.scrollIntoView({ block: 'nearest', inline: 'center', behavior: 'smooth' });
  }

  // ------------------------------------------------------------ 数据加载

  function currentFilter() {
    return {
      keyword: state.query,
      kind: dom.kindFilter.value,
      pinboardId: state.activePinboard,
      sourceApp: dom.appFilter.value,
      limit: 200,
    };
  }

  async function reload() {
    try {
      state.items = await bridge.ListItems(currentFilter());
      if (state.selected >= state.items.length) {
        state.selected = Math.max(0, state.items.length - 1);
      }
      render();
      renderPinboardTabs();
    } catch (err) {
      reportError('加载失败')(err);
    }
  }

  async function loadPinboards() {
    try {
      state.pinboards = await bridge.ListPinboards();
    } catch { state.pinboards = []; }
  }

  async function loadSourceApps() {
    try {
      const apps = await bridge.SourceApps();
      const cur = dom.appFilter.value;
      dom.appFilter.replaceChildren(
        new Option('全部来源', ''),
        ...apps.map((a) => new Option(a, a)),
      );
      dom.appFilter.value = cur;
    } catch { /* 忽略:来源列表是锦上添花 */ }
  }

  // ------------------------------------------------------------ 动作

  async function paste(id, plainText) {
    try {
      await bridge.PasteItem(id, plainText);
    } catch (err) {
      // 回贴失败时后端已经把内容写进剪贴板,提示用户手动粘贴
      reportError('粘贴失败')(err);
    }
  }

  async function togglePin(id) {
    try {
      const pinned = await bridge.TogglePin(id);
      toast(pinned ? '已置顶' : '已取消置顶', 1200);
      await reload();
    } catch (err) { reportError('操作失败')(err); }
  }

  async function removeItem(id) {
    try {
      await bridge.DeleteItem(id);
      await reload();
    } catch (err) { reportError('删除失败')(err); }
  }

  async function openEditor(id) {
    try {
      const text = await bridge.GetItemText(id);
      state.editingId = id;
      dom.editorText.value = text;
      dom.editor.hidden = false;
      dom.editorText.focus();
    } catch (err) { reportError('无法编辑')(err); }
  }

  async function saveEditor() {
    const id = state.editingId;
    if (!id) return;
    try {
      await bridge.UpdateItemContent(id, dom.editorText.value);
      closeEditor();
      await reload();
    } catch (err) { reportError('保存失败')(err); }
  }

  function closeEditor() {
    state.editingId = null;
    dom.editor.hidden = true;
  }

  async function copyToPinboard(itemId) {
    const boards = state.pinboards;
    const names = boards.map((b, i) => `${i + 1}. ${b.name}`).join('\n');
    const answer = prompt(`移动到分组:\n${names}\n\n输入编号(留空则移出分组)`);
    if (answer === null) return;

    const trimmed = answer.trim();
    if (trimmed === '') {
      await bridge.MoveToPinboard(itemId, '').then(reload).catch(reportError('操作失败'));
      return;
    }
    const idx = Number(trimmed) - 1;
    if (!Number.isInteger(idx) || idx < 0 || idx >= boards.length) {
      toast('无效的编号', 1600);
      return;
    }
    await bridge.MoveToPinboard(itemId, boards[idx].id).then(reload).catch(reportError('操作失败'));
  }

  async function togglePause() {
    if (!state.settings) return;
    const next = !state.settings.pauseCapture;
    try {
      await bridge.SetPaused(next);
      state.settings.pauseCapture = next;
      updatePauseButton();
      toast(next ? '已暂停捕获' : '已恢复捕获', 1400);
    } catch (err) { reportError('操作失败')(err); }
  }

  function updatePauseButton() {
    const paused = !!state.settings?.pauseCapture;
    dom.btnPause.textContent = paused ? '已暂停' : '暂停';
    dom.btnPause.classList.toggle('active', paused);
    dom.btnPause.title = paused ? '点击恢复捕获' : '点击暂停捕获';
  }

  // ------------------------------------------------------------ 设置

  function openSettings() {
    const s = state.settings;
    if (!s) return;
    el('set-hotkey').value = s.hotkey;
    el('set-maxitems').value = s.maxItems;
    el('set-panelheight').value = s.panelHeight;
    el('set-autostart').checked = s.autoStart;
    el('set-pasteplain').checked = s.pastePlain;
    el('set-excluded').value = (s.excludedApps ?? []).join('\n');
    dom.settings.hidden = false;
  }

  async function saveSettings() {
    const next = {
      ...state.settings,
      hotkey: el('set-hotkey').value.trim() || state.settings.hotkey,
      maxItems: Number(el('set-maxitems').value) || state.settings.maxItems,
      panelHeight: Number(el('set-panelheight').value) || state.settings.panelHeight,
      autoStart: el('set-autostart').checked,
      pastePlain: el('set-pasteplain').checked,
      excludedApps: el('set-excluded').value
        .split('\n')
        .map((s) => s.trim())
        .filter(Boolean),
    };
    try {
      await bridge.UpdateSettings(next);
      state.settings = await bridge.GetSettings();
      updatePauseButton();
      dom.settings.hidden = true;
      toast('设置已保存', 1500);
    } catch (err) {
      reportError('保存设置失败')(err);
    }
  }

  // ------------------------------------------------------------ 键盘

  function isTyping(target) {
    return target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement;
  }

  /**
   * 这些键永远由面板处理,即使光标停在搜索框里。
   *
   * 搜索框在面板弹出时会自动获得焦点(这是刻意的:弹出即可直接输入搜索)。
   * 但如果把方向键让给输入框,左右浏览就永远不会生效 ——
   * 而它恰恰是这个面板最核心的操作。同理 Enter 必须是"粘贴"而不是换行。
   * 普通字符仍照常输入到搜索框。
   */
  const PANEL_KEYS = new Set([
    'ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Enter', 'Escape',
  ]);

  function onKeyDown(e) {
    // 编辑框与设置面板自己处理按键
    if (!dom.editor.hidden || !dom.settings.hidden) {
      if (e.key === 'Escape') {
        e.preventDefault();
        closeEditor();
        dom.settings.hidden = true;
      }
      return;
    }

    // Ctrl+1..9 快贴 —— 即使光标在搜索框里也应该生效
    if (e.ctrlKey && !e.altKey && !e.shiftKey && /^[1-9]$/.test(e.key)) {
      const idx = Number(e.key) - 1;
      if (idx < state.items.length) {
        e.preventDefault();
        paste(state.items[idx].id, false);
        return;
      }
    }

    if (e.key === 'Escape') {
      e.preventDefault();
      // 搜索框有内容时,Esc 先清空搜索;再按一次才收起面板
      if (isTyping(e.target) && dom.search.value) {
        dom.search.value = '';
        state.query = '';
        reload();
        return;
      }
      clientLog('Esc 收起面板');
      bridge.HidePanel().catch(reportError('收起面板失败'));
      return;
    }

    // 光标在搜索框里时,只截获面板键与 Ctrl 组合键,其余交给输入框
    if (isTyping(e.target) && !PANEL_KEYS.has(e.key) && !e.ctrlKey) {
      return;
    }

    switch (e.key) {
      case 'ArrowRight':
      case 'ArrowDown':
        e.preventDefault();
        select(Math.min(state.selected + 1, state.items.length - 1));
        break;

      case 'ArrowLeft':
      case 'ArrowUp':
        e.preventDefault();
        select(Math.max(state.selected - 1, 0));
        break;

      case 'Home':
        e.preventDefault();
        select(0);
        break;

      case 'End':
        e.preventDefault();
        select(state.items.length - 1);
        break;

      case 'Enter': {
        const item = state.items[state.selected];
        if (!item) return;
        e.preventDefault();
        // Shift+Enter 或配置里的"默认纯文本"都走纯文本粘贴
        const plain = e.shiftKey || !!state.settings?.pastePlain;
        paste(item.id, plain);
        break;
      }

      case 'Delete': {
        // 搜索框里 Delete/Backspace 是编辑文本;只有搜索框为空时才算"删除条目"
        if (isTyping(e.target) && dom.search.value) return;
        const item = state.items[state.selected];
        if (!item) return;
        e.preventDefault();
        removeItem(item.id);
        break;
      }

      default:
        if (e.ctrlKey && e.key.toLowerCase() === 'p') {
          e.preventDefault();
          const item = state.items[state.selected];
          if (item) togglePin(item.id);
        } else if (e.ctrlKey && e.key.toLowerCase() === 'e') {
          e.preventDefault();
          const item = state.items[state.selected];
          if (item) openEditor(item.id);
        } else if (e.ctrlKey && e.key.toLowerCase() === 'b') {
          e.preventDefault();
          const item = state.items[state.selected];
          if (item) copyToPinboard(item.id);
        }
    }
  }

  // ------------------------------------------------------------ 初始化

  function bindEvents() {
    let searchTimer = null;
    dom.search.addEventListener('input', () => {
      clearTimeout(searchTimer);
      // 防抖:每敲一个字符都查库会打断输入
      searchTimer = setTimeout(() => {
        state.query = dom.search.value.trim();
        state.selected = 0;
        reload();
      }, 120);
    });

    dom.searchClear.onclick = () => {
      dom.search.value = '';
      state.query = '';
      dom.search.focus();
      reload();
    };

    dom.kindFilter.onchange = () => { state.selected = 0; reload(); };
    dom.appFilter.onchange = () => { state.selected = 0; reload(); };
    dom.btnPause.onclick = togglePause;
    dom.btnSettings.onclick = openSettings;

    el('editor-cancel').onclick = closeEditor;
    el('editor-save').onclick = saveEditor;
    el('settings-cancel').onclick = () => { dom.settings.hidden = true; };
    el('settings-save').onclick = saveSettings;
    el('settings-opendir').onclick = () => bridge.OpenDataDir().catch(reportError('打开目录失败'));

    document.addEventListener('keydown', onKeyDown);

    // 拖拽把手改面板高度。整个拖拽在 Go 侧完成(轮询鼠标左键状态),
    // 这里只负责发起和给出按下反馈 —— 每次 mousemove 过一次 JS↔Go 桥
    // 会让拖拽明显发涩。
    const grip = el('resize-grip');
    grip.addEventListener('mousedown', (e) => {
      e.preventDefault();
      e.stopPropagation();
      grip.classList.add('active');
      clientLog('按下拖拽把手,开始调整高度');
      bridge.StartPanelResize()
        .catch(reportError('调整面板高度失败'))
        .finally(() => grip.classList.remove('active'));
    });

    // 面板失去焦点时自动收起 —— 点桌面、点别的窗口都应当关掉它,
    // 否则面板常驻置顶,只能靠粘贴或 Esc 才能摆脱,很别扭。
    //
    // 不能直接在 blur 里关:WebView2 在焦点转到原生下拉框等内部元素时
    // 也会触发 window.blur。稍等一拍再用 document.hasFocus() 确认。
    window.addEventListener('blur', () => {
      // 弹层打开时不自动收起,否则编辑到一半切出去就丢了
      if (!dom.editor.hidden || !dom.settings.hidden) return;
      setTimeout(() => {
        if (document.hasFocus()) return;
        clientLog('失去焦点,自动收起面板');
        bridge.HidePanel().catch(() => {});
      }, 150);
    });

    // 卡片轨道是横向滚动容器,而浏览器默认不把纵向滚轮作用到它上面 ——
    // 不转换的话滚轮在面板上完全没反应。这里把 deltaY 映射到 scrollLeft,
    // 同时保留触控板横向滑动(deltaX)的原生行为。
    dom.track.addEventListener('wheel', (e) => {
      if (e.deltaX !== 0) return; // 触控板横滑,交给浏览器
      if (e.deltaY === 0) return;
      e.preventDefault();
      dom.track.scrollLeft += e.deltaY;
    }, { passive: false });

    if (Runtime) {
      Runtime.EventsOn('clip:new', () => { reload(); loadSourceApps(); });
      Runtime.EventsOn('items:changed', () => { reload(); loadPinboards(); });
      Runtime.EventsOn('panel:shown', () => {
        // 面板每次弹出都回到干净状态,避免残留上次的搜索词
        dom.search.value = '';
        state.query = '';
        state.selected = 0;
        dom.kindFilter.value = '';
        dom.appFilter.value = '';
        reload();
        dom.search.focus();
      });
      Runtime.EventsOn('error', (msg) => toast(String(msg), 4000));
      // 托盘菜单里的「设置…」:面板显示后直接把设置弹层打开
      Runtime.EventsOn('panel:settings', () => {
        if (state.settings) openSettings();
      });
    }
  }

  async function init() {
    bindEvents();
    try {
      state.settings = await bridge.GetSettings();
    } catch { state.settings = { pauseCapture: false }; }
    updatePauseButton();

    await loadPinboards();
    await loadSourceApps();
    await reload();
    dom.search.focus();
  }

  init();
})();
