package filewatch

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	DefaultDirPath        = "./logs" // 需要被监控的文件夹
	DefaultFileRegexp     = `.+.log` // 需要被监控的文件名正则表达式
	DefaultCompleteMarker = "LOG_COMPLETE"
)

const (
	CursorFileSuffix = ".cursor"
)

var (
	scanOnce sync.Once // 只扫描一次
)

type FileContent struct {
	FilePath string
	Content  string
	EOF      bool
}

type FileWatcher struct {
	dirPath             string
	fileRegexp          string
	completeMarker      string
	watching            int64
	removeAfterComplete bool
	ResChan             chan FileContent
}

// SetWatchDir 设置监控的文件夹
func (w *FileWatcher) SetWatchDir(dirPath string) {
	w.dirPath = dirPath
}

// SetFileRegexp 设置监控的文件名正则表达式
func (w *FileWatcher) SetFileRegexp(regexp string) {
	w.fileRegexp = regexp
}

// SetCompleteMarker 设置文件的结束标记
func (w *FileWatcher) SetCompleteMarker(marker string) {
	w.completeMarker = marker
}

// SetRemoveAfterComplete 设置监控完毕后是否删除该文件
func (w *FileWatcher) SetRemoveAfterComplete(remove bool) {
	w.removeAfterComplete = remove
}

// GetResChan 获取结果通道
func (w *FileWatcher) GetResChan() <-chan FileContent {
	return w.ResChan
}

// NewWatcher 新建一个watcher, 如果声明多个Watcher, 请自行把控文件夹被重复监控的问题
func NewWatcher() *FileWatcher {
	watcher := &FileWatcher{
		dirPath:             DefaultDirPath,
		fileRegexp:          DefaultFileRegexp,
		completeMarker:      DefaultCompleteMarker,
		removeAfterComplete: false,
		ResChan:             make(chan FileContent),
	}
	return watcher
}

// Start 开始监控任务
func (w *FileWatcher) Start() (err error) {
	if !atomic.CompareAndSwapInt64(&w.watching, 0, 1) {
		fmt.Printf("文件夹(%s)正在被监控中, 无需再起监控任务\n", w.dirPath)
		return nil
	}

	defer func() {
		swapped := atomic.CompareAndSwapInt64(&w.watching, 1, 0)
		fmt.Printf("监控任务结束了, err: %v, 监控状态重置结果: %v\n", err, swapped)
	}()

	// 开始监视文件变更
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("创建watcher失败: %w", err)
	}
	defer watcher.Close()

	// 添加监视的文件夹
	if err := watcher.Add(w.dirPath); err != nil {
		return fmt.Errorf("将文件夹添加至watcher时失败: %w", err)
	}
	if err := filepath.Walk(w.dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// 只添加文件夹和符号链接到监控器
		if info.IsDir() || (info.Mode()&os.ModeSymlink != 0) {
			if err := watcher.Add(path); err != nil {
				return fmt.Errorf("添加文件夹到监控器时失败: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	for {
		select {
		case event := <-watcher.Events:
			if strings.HasSuffix(event.Name, ".cursor") {
				watcher.Remove(event.Name)
				continue
			}
			// 处理文件创建的事件
			if event.Op&fsnotify.Create == fsnotify.Create {
				isDir, err := isDirectory(event.Name)
				if err != nil {
					fmt.Println("判断文件类型失败:", err)
					continue
				}
				if isDir {
					fmt.Printf("将文件夹添加至watcher: %s\n", event.Name)
					watcher.Add(event.Name)
					continue
				}

				filePath := event.Name
				re := regexp.MustCompile(w.fileRegexp)
				// 使用正则表达式提取匹配的子串
				matches := re.FindStringSubmatch(filePath)
				if len(matches) == 0 {
					watcher.Remove(filePath)
					fmt.Printf("非预期的文件: %s, 已忽略监控\n", filePath)
					continue
				}

				go w.Watch(filePath)
			}
		case err := <-watcher.Errors:
			return fmt.Errorf("watcher.Errors: %w", err)
		}
	}
}

// Scan 扫描一次目录, 适用于服务首次或重启时运行一次
func (w *FileWatcher) Scan() {
	scanOnce.Do(
		func() {
			fmt.Println("服务启动时扫描一遍文件目录, 正在将未上报的内容进行上报")
			filepath.Walk(w.dirPath, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					fmt.Printf("遍历文件夹(%v)失败: %v\n", path, err)
					return err
				}

				if strings.HasSuffix(path, CursorFileSuffix) {
					return nil
				}

				if info.IsDir() || (info.Mode()&os.ModeSymlink != 0) {
					return nil
				}

				filePath := path
				re := regexp.MustCompile(w.fileRegexp)
				// 使用正则表达式提取匹配的子串
				matches := re.FindStringSubmatch(filePath)
				if len(matches) > 0 {
					fmt.Printf("Watching: %s\n", path)
					go w.Watch(path)
				}
				return nil
			})
			fmt.Println("文件目录扫描结束")
		},
	)
}

// Watch 对单个文件进行监听
func (w *FileWatcher) Watch(filePath string) (err error) {
	defer func() {
		if err != nil {
			fmt.Println(err)
		}
	}()

	var f *os.File
	f, err = os.OpenFile(filePath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	cursorPath := strings.TrimSuffix(filePath, filepath.Ext(filePath)) + CursorFileSuffix
	offset, _ := readCursor(cursorPath)
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("设置初始seek失败: %w", err)
	}
	fmt.Printf("准备读取文件, file: %s, offset: %d\n", filePath, offset)

	// 创建一个文件监控器
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("创建监控器失败: %w", err)
	}
	defer watcher.Close()
	watcher.Add(filePath)

	maxNoUpdateTime := 4 * time.Hour
	timer := time.NewTicker(maxNoUpdateTime)
	defer timer.Stop()
	fsInfo, err := f.Stat()
	if err != nil {
		return fmt.Errorf("查询文件信息时失败: %w", err)
	}
	longTimeNoUpdate := false
	if time.Since(fsInfo.ModTime()) > maxNoUpdateTime {
		// 长时间不更新认为该任务已停止
		longTimeNoUpdate = true
	}

	go func() {
		// 为了立即读一次, 直接发一个Event触发下
		watcher.Events <- fsnotify.Event{Name: "Read Now", Op: fsnotify.Write}
	}()

	// 监听文件变化事件
	for {
		select {
		case event := <-watcher.Events:
			var eof = false
			// 只关注Write事件，表示文件有新内容
			if event.Op&fsnotify.Write == fsnotify.Write {
				scanner := bufio.NewScanner(f)
				for scanner.Scan() {
					line := scanner.Text()

					eof = line == w.completeMarker
					w.ResChan <- FileContent{FilePath: filePath, Content: line + "\n", EOF: eof}

					// 更新光标位置
					offset, _ := f.Seek(0, io.SeekCurrent)

					// 保存光标信息到配置文件
					err = saveCursor(cursorPath, offset)
					if err != nil {
						// 处理保存光标信息失败的情况
						fmt.Println("Error saving cursor to config:", err)
						continue
					}
					timer.Reset(maxNoUpdateTime)
				}

				if longTimeNoUpdate {
					fmt.Printf("%s 长时间(%v)未更新, 认为文件读取完毕, 不再监控\n", filePath, maxNoUpdateTime)
					return nil
				}
			}
			if event.Op&fsnotify.Remove == fsnotify.Remove {
				fmt.Printf("%s 文件读取完毕\n", filePath)
				return nil
			}

			if eof && w.removeAfterComplete {
				fmt.Printf("%s 文件读取完毕, 开始清理...\n", filePath)
				if err = os.Remove(filePath); err != nil {
					return fmt.Errorf("删除文件(%s)失败: %w", filePath, err)
				}

				if err = os.Remove(cursorPath); err != nil {
					return fmt.Errorf("删除cursor文件失败: %w", err)
				}
				fmt.Printf("文件已被清理: %s、%s\n", filePath, cursorPath)
				return nil
			}
		case e := <-watcher.Errors:
			return fmt.Errorf("watcher.Errors: %w", e)
		case <-timer.C:
			fmt.Printf("%s 长时间(%v)未更新, 认为文件读取完毕, 不再监控\n", filePath, maxNoUpdateTime)
			return nil
		}
	}
}

func readCursor(cursorPath string) (int64, error) {
	data, err := os.ReadFile(cursorPath)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(string(data), 10, 64)
}

func saveCursor(cursorPath string, offset int64) error {
	f, err := os.OpenFile(cursorPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.ModePerm)
	if err != nil {
		return err
	}
	_, err = f.WriteString(fmt.Sprintf("%d", offset))
	return err
}

func isDirectory(path string) (bool, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return fileInfo.IsDir(), nil
}
