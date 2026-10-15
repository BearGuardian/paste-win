// Package exclusion 维护"不记录其复制内容"的应用名单。
//
// 与剪贴板上的敏感标记互补:标记是来源应用主动声明的(密码管理器会做),
// 这里是用户手工指定的兜底,覆盖那些不声明的程序。
package exclusion

import (
	"path/filepath"
	"strings"
	"sync"
)

// List 是线程安全的排除名单。进程名统一按小写比较。
type List struct {
	mu   sync.RWMutex
	apps map[string]struct{}
}

// New 由配置里的名单构造。
func New(apps []string) *List {
	l := &List{}
	l.Replace(apps)
	return l
}

// Replace 整体替换名单。
func (l *List) Replace(apps []string) {
	next := make(map[string]struct{}, len(apps))
	for _, a := range apps {
		if k := normalize(a); k != "" {
			next[k] = struct{}{}
		}
	}
	l.mu.Lock()
	l.apps = next
	l.mu.Unlock()
}

// ShouldSkip 判断来源应用是否应被跳过。
func (l *List) ShouldSkip(app string) bool {
	if app == "" {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.apps[normalize(app)]
	return ok
}

// Apps 返回当前名单(已规范化的形式),供设置界面回显。
func (l *List) Apps() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.apps))
	for a := range l.apps {
		out = append(out, a)
	}
	return out
}

// normalize 把用户输入规整成可比较的进程名。
//
// 用户可能填 "KeePass.exe"、"C:\...\KeePass.exe" 或 "keepass",
// 这些都应该等价。取 basename、去扩展名、转小写。
func normalize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = filepath.Base(s)
	s = strings.TrimSuffix(s, filepath.Ext(s))
	return strings.ToLower(s)
}
