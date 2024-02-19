# filewatch
[English](./README.md)

filewatch是一个文件监控&文件内容收集器。 通常用来监控日志文件，并将内容发送给其他组件

# 快速开始

```go
package main

import (
	"fmt"

	"github.com/ChangSZ/filewatch"
)

func main() {
	watcher := filewatch.NewWatcher()
	watcher.SetCompleteMarker("***")
	watcher.SetFileRegexp(`\d+.log`)
	watcher.SetWatchDir("./logs")
	watcher.SetRemoveAfterComplete(true)

	go func() {
		for info := range watcher.GetResChan() {
			fmt.Printf("%+v\n", info)
		}
	}()
	watcher.Start()
}

```

# 贡献

如果你有一个Issue、发现了一个Bug或者有一个MR, 请不要吝啬, 甩出来！