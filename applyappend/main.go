// applyappend 命令行工具：把补全批次文件（===FILE: 分隔格式）追加进对应笔记。
// 核心逻辑在 bookapply 包（同样被 weread-server 数据更新流程复用），已补全的笔记自动跳过。
// 用法（项目根目录）: go run .\applyappend supplement\batch01.md
package main

import (
	"fmt"
	"os"

	"github.com/qianyuniao/Book-List-Note-System/bookapply"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: applyappend <batchfile>")
		os.Exit(1)
	}
	applied, skipped, err := bookapply.ApplyBatch(".", os.Args[1])
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("applied=%d skipped=%d\n", applied, skipped)
}
