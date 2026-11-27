package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// unescape 把配置里的 \n \r \t \xNN \\ 写法转成真实字节/字符，
// 用于 udp / tcp 探测发送二进制握手包（如 Bedrock 的 raknet ping）。
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(s) {
			out = append(out, '\\')
			break
		}
		switch s[i] {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case '\\':
			out = append(out, '\\')
		case 'x':
			if i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
					out = append(out, byte(v))
					i += 2
					continue
				}
			}
			out = append(out, 'x')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// ---------- Minecraft Server List Ping ----------

// mcVarInt 按协议把 int32 编码成 VarInt（负数固定 5 字节）。
func mcVarInt(v int32) []byte {
	var out [5]byte
	n := 0
	u := uint32(v)
	for {
		if u&^0x7F == 0 {
			out[n] = byte(u)
			n++
			break
		}
		out[n] = byte(u&0x7F) | 0x80
		u >>= 7
		n++
	}
	return out[:n]
}

// mcReadVarInt 从流里读一个 VarInt，最多 5 字节；
// 5 字节形式按 int32 补码解释（协议里负数如 -1 编码成 FF FF FF FF 0F）。
func mcReadVarInt(r io.ByteReader) (int64, error) {
	var result int64
	consumed := 0
	for consumed = 0; consumed < 5; consumed++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= int64(b&0x7F) << uint(7*consumed)
		if b&0x80 == 0 {
			consumed++
			break
		}
	}
	if consumed > 5 {
		return 0, fmt.Errorf("varint too long")
	}
	if consumed == 5 {
		result = int64(int32(result))
	}
	return result, nil
}

func mcWritePacket(w io.Writer, payload []byte) error {
	if _, err := w.Write(mcVarInt(int32(len(payload)))); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func mcReadPacket(r *bufio.Reader, max int) ([]byte, error) {
	length, err := mcReadVarInt(r)
	if err != nil {
		return nil, err
	}
	if length < 0 || length > int64(max) {
		return nil, fmt.Errorf("packet too large: %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// probeMinecraft 走 Server List Ping 协议：拿 MOTD、在线/最大玩家数。
func probeMinecraft(ctx context.Context, cfg map[string]any) ProbeResult {
	host := cfgString(cfg, "host", "")
	port := cfgInt(cfg, "port", 0)
	if host == "" || port < 1 || port > 65535 {
		return ProbeResult{Error: "minecraft channel requires host and port", ConfigError: true}
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))

	var dialer net.Dialer
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		if ctx.Err() != nil {
			return ProbeResult{Error: "timeout"}
		}
		return ProbeResult{Error: "connection failed"}
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)

	// handshake: packetID 0x00 | protocol -1 | host | port | next state 1
	handshake := bytes.Join([][]byte{
		{0x00},
		mcVarInt(-1),
		mcVarInt(int32(len(host))), []byte(host),
		{byte(port >> 8), byte(port)},
		{0x01},
	}, nil)
	if err := mcWritePacket(conn, handshake); err != nil {
		return ProbeResult{Error: "write failed"}
	}
	if err := mcWritePacket(conn, []byte{0x00}); err != nil { // status request
		return ProbeResult{Error: "write failed"}
	}

	reader := bufio.NewReaderSize(conn, 4096)
	packet, err := mcReadPacket(reader, 1<<20)
	if err != nil {
		return ProbeResult{Error: "no response"}
	}
	// packet 内部：packetID 0x00 | VarInt 字符串长度 | JSON
	r := bytes.NewReader(packet)
	if id, err := mcReadVarInt(r); err != nil || id != 0 {
		return ProbeResult{Error: "unexpected response"}
	}
	strLen, err := mcReadVarInt(r)
	if err != nil || strLen <= 0 || strLen > int64(r.Len()) {
		return ProbeResult{Error: "malformed status"}
	}
	body := make([]byte, strLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return ProbeResult{Error: "malformed status"}
	}

	var status struct {
		Version struct {
			Name string `json:"name"`
		} `json:"version"`
		Players struct {
			Online int `json:"online"`
			Max    int `json:"max"`
		} `json:"players"`
		Description json.RawMessage `json:"description"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		return ProbeResult{Error: "malformed status json"}
	}

	motd := ""
	var desc struct{ Text string }
	if json.Unmarshal(status.Description, &desc) == nil && desc.Text != "" {
		motd = desc.Text
	} else {
		motd = strings.Trim(string(status.Description), `"`)
	}
	if expect := cfgString(cfg, "expect", ""); expect != "" && !strings.Contains(motd, expect) {
		return ProbeResult{Error: "MOTD does not contain expected text"}
	}

	players, maxPlayers := status.Players.Online, status.Players.Max
	latency := float64(time.Since(start).Microseconds()) / 1000.0
	return ProbeResult{
		OK: true, StatusCode: 0, LatencyMs: f64(latency),
		Players: &players, MaxPlayers: &maxPlayers,
	}
}

// ---------- Steam / Source A2S_INFO ----------

const (
	a2sHeader      = "\xFF\xFF\xFF\xFF"
	a2sInfoRequest = "TSource Engine Query\x00"
	a2sChallenge   = 0x41
	a2sInfo        = 0x49
	a2sGoldInfo    = 0x6D
)

func a2sBaseRequest() []byte {
	return append([]byte(a2sHeader), []byte(a2sInfoRequest)...)
}

func readCString(buf []byte, pos int) (string, int, bool) {
	if pos >= len(buf) {
		return "", pos, false
	}
	end := bytes.IndexByte(buf[pos:], 0)
	if end < 0 {
		return "", pos, false
	}
	return string(buf[pos : pos+end]), pos + end + 1, true
}

// probeSteam 查询 Source 引擎服务器（CS2/ARK/瓦爾海姆/7 Days 等），拿玩家数。
func probeSteam(ctx context.Context, cfg map[string]any) ProbeResult {
	host := cfgString(cfg, "host", "")
	port := cfgInt(cfg, "port", 0)
	if host == "" || port < 1 || port > 65535 {
		return ProbeResult{Error: "steam channel requires host and port", ConfigError: true}
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		if ctx.Err() != nil {
			return ProbeResult{Error: "timeout"}
		}
		return ProbeResult{Error: "connection failed"}
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)

	start := time.Now()
	request := a2sBaseRequest()
	buf := make([]byte, 4096)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := conn.Write(request); err != nil {
			return ProbeResult{Error: "write failed"}
		}
		n, err := conn.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ProbeResult{Error: "timeout"}
			}
			return ProbeResult{Error: "no response"}
		}
		latency := float64(time.Since(start).Microseconds()) / 1000.0
		reply := buf[:n]
		if len(reply) < 5 || !bytes.HasPrefix(reply, []byte(a2sHeader)) {
			return ProbeResult{LatencyMs: f64(latency), Error: "unexpected response"}
		}
		switch reply[4] {
		case a2sChallenge:
			// 重发请求并带上 4 字节 challenge。
			request = append(a2sBaseRequest(), reply[5:9]...)
			continue
		case a2sInfo, a2sGoldInfo:
			players, maxPlayers, ok := parseA2SPlayers(reply[5:], reply[4] == a2sInfo)
			if !ok {
				return ProbeResult{OK: true, LatencyMs: f64(latency)}
			}
			return ProbeResult{OK: true, LatencyMs: f64(latency), Players: players, MaxPlayers: maxPlayers}
		default:
			return ProbeResult{LatencyMs: f64(latency), Error: "unexpected response type"}
		}
	}
	return ProbeResult{Error: "challenge loop"}
}

func parseA2SPlayers(body []byte, modern bool) (players, maxPlayers *int, ok bool) {
	pos := 0
	if modern {
		pos++ // protocol byte
	}
	for _, _ = range [4]struct{}{} {
		if _, next, good := readCString(body, pos); !good {
			return nil, nil, false
		} else {
			pos = next
		}
	}
	if modern {
		pos += 2 // app id
	}
	if pos+2 > len(body) {
		return nil, nil, false
	}
	online := int(body[pos])
	max := int(body[pos+1])
	return &online, &max, true
}

// ---------- 通用 UDP ----------

// probeUDP 发一段 payload 并（可选）校验回应。 Bedrock/MCPE 这类
// UDP 游戏服可以用 expect 关键字匹配（例如 "MCPE"）。
func probeUDP(ctx context.Context, cfg map[string]any) ProbeResult {
	host := cfgString(cfg, "host", "")
	port := cfgInt(cfg, "port", 0)
	send := unescape(cfgString(cfg, "send", ""))
	expect := cfgString(cfg, "expect", "")
	if host == "" || port < 1 || port > 65535 || send == "" {
		return ProbeResult{Error: "udp channel requires host, port and send", ConfigError: true}
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return ProbeResult{Error: "dial failed"}
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)

	start := time.Now()
	if _, err := conn.Write([]byte(send)); err != nil {
		return ProbeResult{Error: "write failed"}
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	latency := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		if ctx.Err() != nil {
			return ProbeResult{LatencyMs: f64(latency), Error: "timeout"}
		}
		return ProbeResult{LatencyMs: f64(latency), Error: "no response"}
	}
	if expect != "" && !bytes.Contains(buf[:n], []byte(expect)) {
		return ProbeResult{LatencyMs: f64(latency), Error: "unexpected response"}
	}
	return ProbeResult{OK: true, LatencyMs: f64(latency)}
}

// ---------- ICMP ----------

// probeICMP 用系统 ping 命令做连通性探测（免 raw socket 权限）。
func probeICMP(ctx context.Context, cfg map[string]any) ProbeResult {
	host := cfgString(cfg, "host", "")
	if host == "" {
		return ProbeResult{Error: "icmp channel requires host", ConfigError: true}
	}
	timeoutSec := 5
	if deadline, ok := ctx.Deadline(); ok {
		if remain := int(time.Until(deadline).Seconds()); remain > 0 {
			timeoutSec = remain
		}
	}
	cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", strconv.Itoa(timeoutSec), host)
	start := time.Now()
	out, err := cmd.Output()
	latency := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		if ctx.Err() != nil {
			return ProbeResult{LatencyMs: f64(latency), Error: "timeout"}
		}
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg := strings.TrimSpace(strings.SplitN(string(ee.Stderr), "\n", 2)[0])
			return ProbeResult{LatencyMs: f64(latency), Error: truncateErr(msg, 120)}
		}
		return ProbeResult{LatencyMs: f64(latency), Error: "ping failed"}
	}
	rtt := parsePingRTT(string(out))
	if rtt != nil {
		latency = *rtt
	}
	return ProbeResult{OK: true, LatencyMs: f64(latency)}
}

func parsePingRTT(out string) *float64 {
	idx := strings.LastIndex(out, "time=")
	if idx < 0 {
		return nil
	}
	field := out[idx+5:]
	if end := strings.IndexAny(field, " \n"); end >= 0 {
		field = field[:end]
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(field, "ms"), 64)
	if err != nil {
		return nil
	}
	return f64Ptr(v)
}

func truncateErr(msg string, n int) string {
	if len(msg) > n {
		return msg[:n]
	}
	return msg
}

// ---------- DNS ----------

// probeDNS 解析域名（可选校验结果里包含某段 IP），适合登录服/网关域名。
func probeDNS(ctx context.Context, cfg map[string]any) ProbeResult {
	host := cfgString(cfg, "host", "")
	expect := cfgString(cfg, "expect", "")
	if host == "" {
		return ProbeResult{Error: "dns channel requires host", ConfigError: true}
	}
	resolver := net.DefaultResolver
	start := time.Now()
	addrs, err := resolver.LookupHost(ctx, host)
	latency := float64(time.Since(start).Microseconds()) / 1000.0
	if err != nil {
		if ctx.Err() != nil {
			return ProbeResult{LatencyMs: f64(latency), Error: "timeout"}
		}
		return ProbeResult{LatencyMs: f64(latency), Error: "resolution failed"}
	}
	if expect != "" {
		found := false
		for _, a := range addrs {
			if strings.Contains(a, expect) {
				found = true
				break
			}
		}
		if !found {
			return ProbeResult{LatencyMs: f64(latency), Error: "resolved addresses do not match"}
		}
	}
	return ProbeResult{OK: true, LatencyMs: f64(latency)}
}

func f64Ptr(v float64) *float64 { return &v }
