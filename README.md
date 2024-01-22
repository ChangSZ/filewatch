# filewatch
[中文](./README_cn.md)

filewatch is a file harvester, mostly used to fetch logs files and feed them into other components.


## Quick start

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

	watcher.Scan()
	go func() {
		for info := range watcher.GetResChan() {
			fmt.Printf("%+v\n", info)
		}
	}()
	watcher.Start()
}

```


## Contributions

If you have an issue, found a bug or have a feature request, go ahead!