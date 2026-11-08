package permissions

import "strings"

// prefixRunners 会把剩余参数当作命令执行的前缀二进制；不剥离它们的话
// env rm -rf /、nohup shutdown now 之类可以绕过硬拒与风险分级
var prefixRunners = map[string]bool{
	"env": true, "nohup": true, "timeout": true, "nice": true,
	"stdbuf": true, "setsid": true, "command": true, "exec": true,
	"busybox": true, "xargs": true,
}

// stripPrefixRunners 反复剥离前缀执行器包装，返回最终将执行的命令；
// 深度上限 4，与 shell 包装的递归上限一致
func stripPrefixRunners(cmd string) string {
	for i := 0; i < 4; i++ {
		inner, ok := unwrapPrefixRunner(cmd)
		if !ok {
			break
		}
		cmd = normalizeCommand(inner)
	}
	return cmd
}

// unwrapPrefixRunner 提取前缀执行器包装的内层命令；未知二进制不剥离
func unwrapPrefixRunner(cmd string) (string, bool) {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return "", false
	}
	verb := fields[0]
	if verb == "sudo" && len(fields) > 2 {
		// sudo 包装下的前缀执行器同样要穿透
		fields = fields[1:]
		verb = fields[0]
	}
	if !prefixRunners[verb] {
		return "", false
	}
	rest := fields[1:]
	var payload []string
	switch verb {
	case "env":
		payload = skipEnvPrologue(rest)
	case "timeout":
		payload = skipTimeoutPrologue(rest)
	case "nice":
		payload = skipNicePrologue(rest)
	case "stdbuf":
		payload = skipStdbufPrologue(rest)
	case "xargs":
		// xargs 跳过自身选项后，剩余部分整体就是命令前缀
		payload = skipXargsPrologue(rest)
	case "busybox":
		// busybox 后第一个参数是 applet
		payload = rest
	default:
		// nohup、setsid、command、exec 只带无值选项
		payload = skipFlagFields(rest)
	}
	payload = commandPayload(payload)
	if len(payload) == 0 {
		return "", false
	}
	inner := strings.TrimSpace(strings.Join(payload, " "))
	if len(inner) >= 2 && ((inner[0] == '\'' && inner[len(inner)-1] == '\'') ||
		(inner[0] == '"' && inner[len(inner)-1] == '"')) {
		inner = inner[1 : len(inner)-1]
	}
	if inner == "" {
		return "", false
	}
	return inner, true
}

// skipEnvPrologue 跳过 env 的选项与 NAME=VALUE 赋值，返回命令负载；
// 只有选项或赋值（env、env -i、env FOO=1）时返回 nil，保持只读分级
func skipEnvPrologue(rest []string) []string {
	for i := 0; i < len(rest); i++ {
		f := rest[i]
		switch {
		case f == "-u" || f == "--chdir" || f == "--default":
			i++ // 带独立值的选项连同值一起跳过
		case f == "-S" || f == "--split-string":
			return rest[i+1:] // -S 的值本身就是待拆分执行的命令串
		case strings.HasPrefix(f, "-"):
			// -i、-0、--unset=X 等无值或融合式选项
			if v, ok := splitStringArg(f); ok {
				return []string{v}
			}
		case isShellOp(f):
			return nil // 管道或重定向：env 没有命令参数
		case strings.Contains(f, "="):
			// NAME=VALUE 赋值，命令还在后面
		default:
			return rest[i:]
		}
	}
	return nil
}

// splitStringArg 提取融合式 -S/--split-string 选项携带的命令串
func splitStringArg(f string) (string, bool) {
	if strings.HasPrefix(f, "--split-string=") {
		return strings.TrimPrefix(f, "--split-string="), true
	}
	if strings.HasPrefix(f, "-S") && len(f) > 2 {
		return f[2:], true
	}
	return "", false
}

// skipTimeoutPrologue 跳过 timeout 的选项与时长参数，返回命令负载
func skipTimeoutPrologue(rest []string) []string {
	for i := 0; i < len(rest); i++ {
		f := rest[i]
		if !strings.HasPrefix(f, "-") {
			return rest[i+1:] // 时长参数之后的剩余部分是命令
		}
		switch f {
		case "-s", "--signal", "-k", "--kill-after":
			i++ // 选项值一并跳过
		}
	}
	return nil
}

// skipNicePrologue 跳过 nice 的优先级调整项，返回命令负载
func skipNicePrologue(rest []string) []string {
	for i := 0; i < len(rest); i++ {
		f := rest[i]
		switch {
		case f == "-n" || f == "--adjustment":
			i++ // 调整值一并跳过
		case strings.HasPrefix(f, "-") && len(f) > 1:
			// -N 融合式调整或长选项
		default:
			return rest[i:]
		}
	}
	return nil
}

// skipStdbufPrologue 跳过 stdbuf 的缓冲区选项及其参数值，返回命令负载
func skipStdbufPrologue(rest []string) []string {
	for i := 0; i < len(rest); i++ {
		f := rest[i]
		switch {
		case f == "-o" || f == "-i" || f == "-e" ||
			f == "--output" || f == "--input" || f == "--error":
			i++ // 模式值一并跳过
		case strings.HasPrefix(f, "-"):
			// -oL、--output=L 等融合式选项
		default:
			return rest[i:]
		}
	}
	return nil
}

// skipXargsPrologue 跳过 xargs 自身的选项，返回命令负载
func skipXargsPrologue(rest []string) []string {
	for i := 0; i < len(rest); i++ {
		f := rest[i]
		switch {
		case f == "-I" || f == "-L" || f == "-n" || f == "-P" ||
			f == "-s" || f == "-d" || f == "-E":
			i++ // 带独立值的选项连同值一起跳过
		case strings.HasPrefix(f, "-"):
			// -0、-r、-t、--replace=X 等无值或融合式选项
		default:
			return rest[i:]
		}
	}
	return nil
}

// skipFlagFields 跳过开头的无值选项字段
func skipFlagFields(rest []string) []string {
	for i := 0; i < len(rest); i++ {
		if !strings.HasPrefix(rest[i], "-") {
			return rest[i:]
		}
	}
	return nil
}

// commandPayload 校验负载首字段不是 shell 操作符，是则视为无命令
func commandPayload(payload []string) []string {
	if len(payload) > 0 && isShellOp(payload[0]) {
		return nil
	}
	return payload
}

// isShellOp 判断字段是否为纯 shell 操作符（管道、分隔、重定向）
func isShellOp(f string) bool {
	if f == "" {
		return false
	}
	for i := 0; i < len(f); i++ {
		switch f[i] {
		case '|', ';', '&', '>', '<', '(', ')':
		default:
			return false
		}
	}
	return true
}
