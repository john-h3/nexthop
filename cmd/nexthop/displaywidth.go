package main

import "strings"

// displayWidth 计算字符串在终端中的显示宽度（CJK/全角字符占 2 列）。
// Go 的 fmt %-Ns 按字符数补空格，中文等宽字符会导致对齐错位，必须用显示宽度对齐。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if isWideRune(r) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// isWideRune 判断是否为宽字符（Unicode East Asian Width W/F 的常见范围）。
func isWideRune(r rune) bool {
	return r >= 0x1100 && (r <= 0x115F || // Hangul Jamo
		r == 0x2329 || r == 0x232A || // 数学尖括号
		(r >= 0x2E80 && r <= 0xA4CF && r != 0x303F) || // CJK 等
		(r >= 0xAC00 && r <= 0xD7A3) || // Hangul 音节
		(r >= 0xF900 && r <= 0xFAFF) || // CJK 兼容表意
		(r >= 0xFE30 && r <= 0xFE4F) || // CJK 兼容形式
		(r >= 0xFF00 && r <= 0xFF60) || // 全角形式
		(r >= 0xFFE0 && r <= 0xFFE6)) // 全角符号
}

// padRight 按显示宽度右补空格到指定宽度（宽字符按 2 列计）。
func padRight(s string, width int) string {
	w := displayWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}
