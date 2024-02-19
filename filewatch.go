package filewatch

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	DefaultDirPath         = "./logs"       // 需要被监控的文件夹
	DefaultFileRegexp      = `.+.log`       // 需要被监控的文件名正则表达式
	DefaultCompleteMarker  = "LOG_COMPLETE" // 文件监控结束标志符
	DefaultMaxNoUpdateTime = 4 * time.Hour  // 文件最大未更新时长
)

const (
	CursorFileSuffix = ".cursor"
)

type FileContent struct {
	FilePath string
	Content  []byte
	EOF      bool
}

func (f FileContent) String() string {
	return fmt.Sprintf("filePath: %v, Content: %s, EOF: %v", f.FilePath, f.Content, f.EOF)
}

type FileWatcher struct {
	dirPath             string
	fileRegexp          string
	completeMarker      string
	watching            int64
	removeAfterComplete bool
	maxNoUpdateTime     time.Duration
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

// SetMaxNoUpdateTime 设置文件最大未更新时间, 用来结束监控协程
func (w *FileWatcher) SetMaxNoUpdateTime(dur time.Duration) {
	w.maxNoUpdateTime = dur
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
		maxNoUpdateTime:     DefaultMaxNoUpdateTime,
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

	go w.Scan()
	defer func() {
		if err == fsnotify.ErrEventOverflow {
			go w.Start()
		}
	}()

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

// Scan 扫描一次目录
func (w *FileWatcher) Scan() {
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
}

// Watch 对单个文件进行监听
func (w *FileWatcher) Watch(filePath string) (err error) {
	defer func() {
		if err != nil {
			fmt.Println(err)
		}
		fmt.Printf("%s 文件内容监听结束\n", filePath)
	}()

	var f *os.File
	f, err = os.OpenFile(filePath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	cursorFile := strings.TrimSuffix(filePath, filepath.Ext(filePath)) + CursorFileSuffix
	offset, _ := readCursor(cursorFile)
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("设置初始seek失败: %w", err)
	}
	fmt.Printf("准备读取文件, file: %s, offset: %d\n", filePath, offset)

	// 打开游标文件写
	var cursorFW *os.File
	cursorFW, err = os.OpenFile(cursorFile, os.O_WRONLY|os.O_CREATE, os.ModePerm)
	if err != nil {
		return fmt.Errorf("打开游标文件失败: %w", err)
	}
	defer cursorFW.Close()

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

	scanChan := make(chan bool, 2)
	go w.watchFileEvent(filePath, scanChan)

	// 计时器, 2秒内至少发送一次
	maxSendDur := 2 * time.Second
	sendTimer := time.NewTicker(maxSendDur)
	defer sendTimer.Stop()

	const maxBatchCnt = 1000
	var batchLog = bytes.NewBuffer(make([]byte, 0, 1024*1024)) // 申请1M容量
	var batchCnt int
	for {
		select {
		case ifScan := <-scanChan:
			if !ifScan { // false表示不需要再扫描了
				return nil
			}
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				batchCnt++
				line := scanner.Bytes()
				// 更新光标位置
				offset, _ = f.Seek(0, io.SeekCurrent)

				eof := string(line) == w.completeMarker
				line = append(line, '\n')
				batchLog.Write(line)
				if eof || batchCnt >= maxBatchCnt {
					w.ResChan <- FileContent{FilePath: filePath, Content: batchLog.Bytes(), EOF: eof}
					batchLog.Reset()
					batchCnt = 0
					sendTimer.Reset(maxSendDur)

					// 保存光标信息到配置文件
					err = saveCursor(cursorFW, offset)
					if err != nil {
						// 处理保存光标信息失败的情况
						fmt.Println("Error saving cursor to config:", err)
					}
				}
				if eof {
					fmt.Printf("%s 文件读取完毕, 开始清理...\n", filePath)
					if err = os.Remove(filePath); err != nil {
						fmt.Printf("删除log文件失败: %v\n", err)
						return
					}
					if err = os.Remove(cursorFile); err != nil {
						fmt.Printf("删除cursor文件失败: %v\n", err)
						return
					}
					fmt.Printf("%s '.log'、'.cursor'文件清理完毕\n", strings.TrimSuffix(filePath, ".log"))
					return
				}
			}
			if scanner.Err() != nil {
				fmt.Printf("扫描文件(%s)时发生错误: %v\n", filePath, err)
			}
		case <-sendTimer.C:
			if batchLog.Len() > 0 {
				w.ResChan <- FileContent{FilePath: filePath, Content: batchLog.Bytes(), EOF: false}
				batchLog.Reset()
				batchCnt = 0

				// 保存光标信息到配置文件
				err = saveCursor(cursorFW, offset)
				if err != nil {
					// 处理保存光标信息失败的情况
					fmt.Println("Error saving cursor to config:", err)
					continue
				}
			}

			if longTimeNoUpdate {
				fmt.Printf("%s 长时间(%v)未更新, 认为文件读取完毕, 不再监控\n", filePath, maxNoUpdateTime)
				return nil
			}
			sendTimer.Reset(maxSendDur)
		}
	}
}

func (w *FileWatcher) watchFileEvent(filePath string, scanChan chan bool) {
	defer fmt.Printf("%s 文件事件监听完成\n", filePath)
	// 创建一个文件监控器
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Printf("%s 文件创建监控器失败: %v\n", err, filePath)
		scanChan <- false
		return
	}
	defer watcher.Close()
	watcher.Add(filePath)

	go func() {
		// 为了立即读一次, 直接发一个Event触发下
		watcher.Events <- fsnotify.Event{Name: "Read Now", Op: fsnotify.Write}
	}()

	timer := time.NewTicker(w.maxNoUpdateTime)
	defer timer.Stop()

	// 监听文件变化事件
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				fmt.Printf("%s watcher.Events被关闭了\n", filePath)
				scanChan <- false
				return
			}
			// 只关注Write事件，表示文件有新内容
			if event.Op&fsnotify.Write == fsnotify.Write {
				if len(scanChan) <= 1 {
					scanChan <- true
				}
				timer.Reset(w.maxNoUpdateTime)
			}
			if event.Op&fsnotify.Remove == fsnotify.Remove {
				fmt.Printf("%s 文件读取完毕\n", filePath)
				scanChan <- false
				return
			}
		case e := <-watcher.Errors:
			fmt.Printf("watcher.Errors: %v\n", e)
			scanChan <- false
			return
		case <-timer.C:
			fmt.Printf("%s 长时间(%v)未更新, 认为文件读取完毕, 不再监控\n", filePath, w.maxNoUpdateTime)
			scanChan <- false
			return
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

func saveCursor(f *os.File, offset int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := f.WriteString(fmt.Sprintf("%d", offset))
	return err
}

func isDirectory(path string) (bool, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return fileInfo.IsDir(), nil
}
