package compress

import "testing"

// TestNegotiate 覆盖 Accept-Encoding 协商语义: q 值, 通配, 拒绝, 大小写, 空白与别名。
func TestNegotiate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"header missing", "", Identity},
		{"header blank", "   ", Identity},
		{"single encoding", "gzip", Gzip},
		{"list without q", "deflate, gzip, br", Gzip},
		{"q picks best by server preference", "zstd;q=0.9, gzip;q=0.8, deflate;q=0.7", Gzip},
		{"server preference beats client order", "zstd, gzip", Gzip},
		{"explicit zero rejects despite wildcard", "gzip;q=0, *", Zstd},
		{"zero on preferred falls through", "br;q=0, gzip", Gzip},
		{"all rejected yields identity", "gzip;q=0, br;q=0, zstd;q=0, deflate;q=0", Identity},
		{"wildcard alone picks first preference", "*", Gzip},
		{"zero wildcard blocks unlisted encodings", "*;q=0, gzip;q=0.5", Gzip},
		{"wildcard rescue after explicit zeros", "gzip;q=0, br;q=0, *;q=0.2", Zstd},
		{"case insensitive tokens and q name", "GZip; Q=0.5, BR", Gzip},
		{"whitespace tolerated", "  gzip ; q=0.3 ,  zstd ", Gzip},
		{"x-gzip alias accepted", "x-gzip", Gzip},
		{"duplicate takes minimal q", "gzip;q=0.9, gzip;q=0.2, *", Gzip},
		{"duplicate conflicting zero rejects", "gzip;q=0.9, gzip;q=0", Identity},
		{"illegal q rejects that encoding", "br;q=abc, gzip", Gzip},
		{"out of range q rejects that encoding", "br;q=1.5, gzip", Gzip},
		{"identity entries ignored", "identity;q=0, gzip", Gzip},
		{"parameters other than q ignored", "gzip;q=1.0, foo=bar", Gzip},
		{"extra semicolon fields tolerated", "gzip;;;", Gzip},
		{"empty entries tolerated", ", , gzip, ,", Gzip},
		{"q equals one explicit", "gzip;q=1", Gzip},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Negotiate(tt.header); got != tt.want {
				t.Fatalf("Negotiate(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}
