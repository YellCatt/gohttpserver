package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/go-yaml/yaml"
)

const (
	hotReloadInterval = 30 * time.Second
	configFileName    = "config.yaml"
)

type atomicLevelHandler struct {
	slog.Handler
	level *slog.AtomicLevel
}

func (h *atomicLevelHandler) Enabled(r slog.Level) bool {
	return r >= h.level.Level()
}

func (h *atomicLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &atomicLevelHandler{Handler: h.Handler.WithAttrs(attrs), level: h.level}
}

func (h *atomicLevelHandler) WithGroup(name string) slog.Handler {
	return &atomicLevelHandler{Handler: h.Handler.WithGroup(name), level: h.level}
}

var (
	slogLevel   = slog.LevelInfo
	levelHolder *slog.AtomicLevel
	configMTime time.Time
)

func reloadConfig() bool {
	info, err := os.Stat(configFileName)
	if err != nil {
		if os.IsNotExist(err) {
			debugLog("热加载: 配置文件不存在，跳过")
			return false
		}
		warnLog("热加载: 读取配置文件stat失败: %v", err)
		return false
	}

	modTime := info.ModTime()
	if !modTime.After(configMTime) {
		return false
	}

	debugLog("热加载: 检测到配置文件变更，重新加载...")

	oldCfg := gcfg
	oldDebug := gcfg.Debug

	data, err := os.ReadFile(configFileName)
	if err != nil {
		warnLog("热加载: 读取配置文件失败: %v", err)
		return false
	}

	newCfg := Configure{}
	if err := yaml.Unmarshal(data, &newCfg); err != nil {
		warnLog("热加载: 解析配置文件失败: %v", err)
		return false
	}

	gcfg = newCfg
	configMTime = modTime

	changed := false

	if gcfg.Debug != oldDebug {
		changed = true
		if gcfg.Debug {
			slogLevel = slog.LevelDebug
		} else {
			slogLevel = slog.LevelInfo
		}
		levelHolder.SetLevel(slogLevel)
		infoLog("热加载: 日志级别切换为 %v", slogLevel)
	}

	if gcfg.Theme != oldCfg.Theme {
		changed = true
		ss.Theme = gcfg.Theme
		infoLog("热加载: 主题切换为 %s", gcfg.Theme)
	}

	if gcfg.Title != oldCfg.Title {
		changed = true
		ss.Title = gcfg.Title
		infoLog("热加载: 标题切换为 %s", gcfg.Title)
	}

	if gcfg.Upload != oldCfg.Upload {
		changed = true
		ss.Upload = gcfg.Upload
		infoLog("热加载: 上传权限切换为 %v", gcfg.Upload)
	}

	if gcfg.Delete != oldCfg.Delete {
		changed = true
		ss.Delete = gcfg.Delete
		infoLog("热加载: 删除权限切换为 %v", gcfg.Delete)
	}

	if gcfg.DeepPathMaxDepth != oldCfg.DeepPathMaxDepth {
		changed = true
		ss.DeepPathMaxDepth = gcfg.DeepPathMaxDepth
		infoLog("热加载: 深层目录深度切换为 %d", gcfg.DeepPathMaxDepth)
	}

	if gcfg.NoIndex != oldCfg.NoIndex {
		changed = true
		ss.NoIndex = gcfg.NoIndex
		infoLog("热加载: 索引开关切换为 %v", gcfg.NoIndex)
	}

	if gcfg.Root != oldCfg.Root {
		changed = true
		ss.Root = gcfg.Root
		infoLog("热加载: 根目录切换为 %s", gcfg.Root)
	}

	if gcfg.Prefix != oldCfg.Prefix {
		changed = true
		ss.Prefix = fixPrefix(gcfg.Prefix)
		infoLog("热加载: URL前缀切换为 %s", gcfg.Prefix)
	}

	if gcfg.PlistProxy != oldCfg.PlistProxy {
		changed = true
		ss.PlistProxy = gcfg.PlistProxy
		infoLog("热加载: Plist代理切换为 %s", gcfg.PlistProxy)
	}

	if changed {
		infoLog("热加载: 配置文件热加载完成")
	}
	return changed
}

func startHotReload() {
	ticker := time.NewTicker(hotReloadInterval)
	defer ticker.Stop()
	for range ticker.C {
		reloadConfig()
	}
}