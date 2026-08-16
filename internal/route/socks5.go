package route

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"scopeforge/internal/traffic"
)

// socks5Server 是内置 SOCKS5 服务器(RFC 1928,NO AUTH + CONNECT)。
// 无外部依赖;CONNECT 目标经 traffic.Recorder 记录(审计)。
// 出口边界:元数据端点(169.254.169.254/metadata.google.internal)永远拒绝
// (docs/04 §5.3;目标白名单由 ScopeGate 在工具层控制)。
type socks5Server struct {
	ln        net.Listener
	recorder  *traffic.Recorder
	challenge string
	blockMeta bool
}

func startSocks5(addr string, rec *traffic.Recorder, challenge string) (*socks5Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &socks5Server{ln: ln, recorder: rec, challenge: challenge, blockMeta: true}
	go s.serve()
	return s, nil
}

func (s *socks5Server) addr() string { return s.ln.Addr().String() }
func (s *socks5Server) close()       { s.ln.Close() }

// healthy 探测:握手一个空连接(能连上即认为健康)。
func (s *socks5Server) healthy() bool {
	conn, err := net.DialTimeout("tcp", s.addr(), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (s *socks5Server) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *socks5Server) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	// 握手:版本 + 方法协商
	ver := make([]byte, 2)
	if _, err := io.ReadFull(conn, ver); err != nil || ver[0] != 0x05 {
		return
	}
	methods := make([]byte, ver[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	// 仅 NO AUTH(0x00)
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	// 请求:VER CMD RSV ATYP ...
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	if hdr[0] != 0x05 || hdr[1] != 0x01 { // 仅 CONNECT
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	host, err := readAddr(conn, hdr[3])
	if err != nil {
		return
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBuf)

	// 出口边界:元数据端点永远拒绝(docs/04 §5.3)
	if s.blockMeta && traffic.BlockedMeta(net.JoinHostPort(host, strconv.Itoa(int(port)))) {
		conn.Write([]byte{0x05, 0x02, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // 拒绝(规则不允许)
		return
	}

	// 连接目标
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), 10*time.Second)
	if err != nil {
		conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()

	// 成功应答
	local := upstream.LocalAddr().(*net.TCPAddr)
	resp := []byte{0x05, 0x00, 0x00, 0x01}
	ip4 := local.IP.To4()
	resp = append(resp, ip4...)
	resp = binary.BigEndian.AppendUint16(resp, uint16(local.Port))
	if _, err := conn.Write(resp); err != nil {
		return
	}

	// 流量审计(CONNECT 元数据;内容为字节流,HTTP 明文捕获走 http 出口)
	if s.recorder != nil {
		s.recorder.Record(traffic.Flow{Method: "CONNECT", Host: net.JoinHostPort(host, strconv.Itoa(int(port))), Status: 200})
	}

	_ = conn.SetDeadline(time.Time{})
	go io.Copy(upstream, conn)
	io.Copy(conn, upstream)
}

// readAddr 解析 ATYP:1=IPv4, 3=域名, 4=IPv6。
func readAddr(conn net.Conn, atyp byte) (string, error) {
	switch atyp {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		return string(b), nil
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	default:
		return "", fmt.Errorf("bad atyp %d", atyp)
	}
}

var _ = context.Background
