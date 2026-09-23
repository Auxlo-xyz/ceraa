package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger maps to soraacode observability/logging.ts + utils/logger.js
type Logger struct {
	mu   sync.Mutex
	file *os.File
	path string
}

var defaultLogger *Logger
var initOnce sync.Once

// Initialize creates logger at path like ~/.soraacode/logs/bridge.log
// Maps to soraacode initializeLogger + flushLogger
func Initialize(path string) (*Logger, error) {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".config", "sooraa", "logs", "bridge.log")
		// Also try soraacode path for compat
		if _, err := os.Stat(filepath.Join(home, ".soraacode", "logs", "bridge.log")); err == nil {
			path = filepath.Join(home, ".soraacode", "logs", "bridge.log")
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	l := &Logger{file: f, path: path}
	defaultLogger = l
	return l, nil
}

func Get() *Logger {
	if defaultLogger == nil {
		// Lazy init
		Initialize("")
	}
	return defaultLogger
}

func (l *Logger) log(level, msg string) {
	if l == nil || l.file == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(l.file, "[%s] %s: %s\n", ts, level, msg)
}

func (l *Logger) Info(msg string)  { l.log("INFO", msg) }
func (l *Logger) Warn(msg string)  { l.log("WARN", msg) }
func (l *Logger) Error(msg string) { l.log("ERROR", msg) }
func (l *Logger) Debug(msg string) { l.log("DEBUG", msg) }

func (l *Logger) Flush() error {
	if l == nil || l.file == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Sync()
}

func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}

// Global helpers like soraacode logger.js
func Info(msg string)  { Get().Info(msg) }
func Warn(msg string)  { Get().Warn(msg) }
func Error(msg string) { Get().Error(msg) }
func Debug(msg string) { Get().Debug(msg) }
func Flush() error     { return Get().Flush() }

// SafeBackgroundTask runs fn in goroutine and logs panic, maps to soraacode utils/safe-background-task.js
func SafeBackgroundTask(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				Error(fmt.Sprintf("background task %s panic: %v", name, r))
			}
		}()
		fn()
	}()
}
