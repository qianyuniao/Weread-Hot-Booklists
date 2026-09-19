// Package bookapply 把手工补全批次文件追加进对应笔记。
//
// 批次文件格式：以 "===FILE: 相对路径" 行分隔，其后为要追加的正文（### 内容简介 等章节）。
// 幂等保护：笔记中已存在 "### 内容简介" 标记时跳过，防止重复追加——
// 因此爬虫重写 books/ 之后重放批次即可安全恢复补全内容。
// 由 applyappend 命令行工具与 weread-server 的数据更新流程共同复用。
package bookapply

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Marker 判定笔记是否已含补全内容的标记
const Marker = "### 内容简介"

// ApplyBatch 应用单个批次文件，root 为项目根目录；返回成功追加与跳过（缺失/已补全）的数量。
func ApplyBatch(root, batchFile string) (applied, skipped int, err error) {
	data, err := os.ReadFile(batchFile)
	if err != nil {
		return 0, 0, fmt.Errorf("读取批次文件失败: %w", err)
	}
	parts := strings.Split(string(data), "===FILE:")
	for _, p := range parts[1:] {
		lines := strings.SplitN(strings.TrimPrefix(p, "\n"), "\n", 2)
		rel := strings.TrimSpace(lines[0])
		body := ""
		if len(lines) > 1 {
			body = lines[1]
		}
		if rel == "" || body == "" {
			continue
		}
		// 批次文件里的路径可能带 Windows 反斜杠，统一转正斜杠后再拼到 root 下
		path := filepath.Join(root, filepath.FromSlash(filepath.ToSlash(rel)))
		existing, e := os.ReadFile(path)
		if e != nil {
			skipped++
			continue
		}
		if strings.Contains(string(existing), Marker) {
			skipped++ // 已补全过，避免重复追加
			continue
		}
		f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
		if e != nil {
			skipped++
			continue
		}
		// 确保以空行分隔原内容与新正文
		r := bufio.NewReader(strings.NewReader(body))
		first, _ := r.ReadString('\n')
		if !strings.HasPrefix(first, "### ") {
			f.WriteString("\n")
		}
		f.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			f.WriteString("\n")
		}
		f.Close()
		applied++
	}
	return applied, skipped, nil
}
