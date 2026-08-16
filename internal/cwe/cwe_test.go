package cwe

import "testing"

func TestNormalizeCWE(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"CWE-89", "CWE-89", true},
		{"cwe-89", "CWE-89", true},
		{"CWE89", "CWE-89", true},
		{"89", "CWE-89", true},
		{"CWE-089", "CWE-89", true}, // 前导零归一
		{"CWE-79", "CWE-79", true},
		{"CWE-9999", "", false}, // 编号合法但不在白名单
		{"CWE-XX", "", false},   // 非法格式
		{"sqli", "", false},     // 非编号
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeCWE(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeCWE(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
	// 白名单覆盖常用编号(服务端校验不空转)
	for _, id := range []string{"CWE-89", "CWE-79", "CWE-22", "CWE-352", "CWE-918", "CWE-434", "CWE-502", "CWE-611", "CWE-639"} {
		if !IsKnown(id) {
			t.Errorf("IsKnown(%s) = false, want true", id)
		}
	}
}

func TestNormalizeAsset(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Shop.Example.com", "shop.example.com"},
		{"https://shop.example.com/login", "shop.example.com"},
		{"http://www.example.com", "example.com"},
		{"WWW.Example.COM", "example.com"},
		{"10.10.0.7", "10.10.0.7"},
		{"https://user:pass@example.com/", "example.com"},
		{"  example.com  ", "example.com"},
	}
	for _, c := range cases {
		if got := NormalizeAsset(c.in); got != c.want {
			t.Errorf("NormalizeAsset(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/login", "/login"},
		{"/login/", "/login"},
		{"/search?q=1&page=2", "/search"},
		{"/api/order%20list", "/api/order list"}, // decode
		{"/", "/"},
		{"", ""},
		{"login", "login"},
		// 实测形态:模型把方法前缀或完整 URL 当 endpoint 提交(聚焦过滤/账本
		// 去重键必须归一,否则误杀/重复)
		{"GET /rest/products/search?q=", "/rest/products/search"},
		{"POST /rest/products/search", "/rest/products/search"},
		{"http://192.168.81.167:3000/rest/products/search?q=", "/rest/products/search"},
		{"GET http://192.168.81.167:3000/rest/products/search?q=", "/rest/products/search"},
		{"https://shop.example.com/login/", "/login"},
	}
	for _, c := range cases {
		if got := NormalizeEndpoint(c.in); got != c.want {
			t.Errorf("NormalizeEndpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
