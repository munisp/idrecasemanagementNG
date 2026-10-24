// Author: Patrick Munis at newwave
package main

// Upload security: malware scanning (ClamAV INSTREAM), content-policy checks
// (magic-byte executable detection, extension blocklist, type mismatch), and
// abuse throttling (per-IP / per-token rate limits, per-link file and byte
// budgets). Every byte that enters the platform — authenticated uploads and
// anonymous ShareBox links alike — passes this layer BEFORE it is sealed and
// stored. Fail-closed: if the scanner is down, uploads are refused (503),
// never silently accepted.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// ---- ClamAV (INSTREAM over TCP) ----------------------------------------------

// clamScan streams data to clamd's INSTREAM command and returns the detected
// signature name ("" = clean). Works for whole blobs and streamed parts.
func (s *server) clamScan(r io.Reader) (string, error) {
	if s.cfg.ClamdAddr == "" {
		return "", fmt.Errorf("scanner not configured")
	}
	conn, err := net.DialTimeout("tcp", s.cfg.ClamdAddr, 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("scanner unreachable: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return "", err
	}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			var sz [4]byte
			binary.BigEndian.PutUint32(sz[:], uint32(n))
			if _, err := conn.Write(sz[:]); err != nil {
				return "", err
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return "", err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil { // terminator
		return "", err
	}
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	resp = strings.TrimSpace(resp)
	// "stream: OK" | "stream: <Signature> FOUND" | "stream: <error> ERROR"
	switch {
	case strings.HasSuffix(resp, " OK"):
		return "", nil
	case strings.HasSuffix(resp, " FOUND"):
		return strings.TrimSuffix(strings.TrimPrefix(resp, "stream: "), " FOUND"), nil
	default:
		return "", fmt.Errorf("scanner: %s", resp)
	}
}

// scanOrRefuse scans upload bytes; fail-closed when the scanner is down.
// Returns the signature if infected, "" if clean; error only on scanner outage.
func (s *server) scanOrRefuse(w http.ResponseWriter, data io.Reader, what string) (string, bool) {
	sig, err := s.clamScan(data)
	if err != nil {
		http.Error(w, `{"error":"malware scanner unavailable — upload refused (fail-closed)"}`, http.StatusServiceUnavailable)
		return "", false
	}
	if sig != "" {
		http.Error(w, fmt.Sprintf(`{"error":"rejected: malware detected (%s) — %s quarantined"}`, sig, what), http.StatusUnprocessableEntity)
		return sig, false
	}
	return "", true
}

// ---- Content policy ------------------------------------------------------------

var blockedExtensions = map[string]bool{
	".exe": true, ".dll": true, ".scr": true, ".bat": true, ".cmd": true,
	".ps1": true, ".vbs": true, ".js": true, ".jar": true, ".msi": true,
	".com": true, ".pif": true, ".lnk": true, ".hta": true, ".wsf": true,
	".sh": true, ".apk": true, ".app": true, ".deb": true, ".rpm": true,
}

// contentPolicy rejects executables by magic bytes and extension, and flags
// polyglot tricks (double extensions like bill.pdf.exe).
func contentPolicy(filename string, head []byte) error {
	lower := strings.ToLower(filename)
	// double-extension evasion: anything.exe/.bat/… anywhere in the name
	for ext := range blockedExtensions {
		if strings.Contains(lower, ext) {
			return fmt.Errorf("executable content is not accepted (%s)", ext)
		}
	}
	if blockedExtensions[strings.ToLower(filepath.Ext(lower))] {
		return fmt.Errorf("file type not accepted")
	}
	// magic-byte executable detection (first 512 bytes)
	if len(head) >= 4 {
		switch {
		case head[0] == 'M' && head[1] == 'Z': // PE/COFF (Windows exe/dll)
			return fmt.Errorf("Windows executable content is not accepted")
		case head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F':
			return fmt.Errorf("ELF executable content is not accepted")
		case head[0] == 0xcf && head[1] == 0xfa: // Mach-O
			return fmt.Errorf("Mach-O executable content is not accepted")
		case head[0] == '#' && head[1] == '!': // script shebang
			return fmt.Errorf("script content is not accepted")
		}
	}
	// Allowlist: only document/image types the platform can actually process
	// are stored at all. Anything else — video, audio, archives, unknown
	// binaries — is refused here, BEFORE it consumes encryption, object
	// storage, queue capacity, or doc-intel worker time.
	return allowedDocType(lower, head)
}

// allowedDocExts: extensions the doc-intel pipeline can parse (Docling set).
var allowedDocExts = map[string]string{
	".pdf": "pdf", ".docx": "office", ".xlsx": "office", ".pptx": "office",
	".png": "image", ".jpg": "image", ".jpeg": "image", ".tif": "image",
	".tiff": "image", ".bmp": "image", ".gif": "image", ".webp": "image",
	".txt": "text", ".csv": "text", ".md": "text", ".html": "text", ".htm": "text",
}

// sniffKind classifies content by magic bytes: pdf / office (ZIP container) /
// image / text / unknown. "unknown" is always rejected.
func sniffKind(head []byte) string {
	if len(head) >= 5 && string(head[:5]) == "%PDF-" {
		return "pdf"
	}
	if len(head) >= 4 && head[0] == 'P' && head[1] == 'K' && head[2] == 3 && head[3] == 4 {
		return "office" // ZIP container — only OOXML extensions permitted
	}
	if len(head) >= 8 && string(head[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image"
	}
	if len(head) >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff {
		return "image" // JPEG
	}
	if len(head) >= 4 && (string(head[:4]) == "II*\x00" || string(head[:4]) == "MM\x00*") {
		return "image" // TIFF
	}
	if len(head) >= 2 && string(head[:2]) == "BM" {
		return "image" // BMP
	}
	if len(head) >= 4 && string(head[:4]) == "GIF8" {
		return "image"
	}
	if len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		return "image"
	}
	// Text heuristic: no NUL bytes and overwhelmingly printable in the first
	// 4 KB. Catches .txt/.csv/.md/.html which have no magic number.
	sample := head
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	if len(sample) > 0 {
		printable := 0
		for _, b := range sample {
			if b == 0 {
				return "unknown" // NUL => binary masquerading as text
			}
			if b == '\t' || b == '\n' || b == '\r' || (b >= 0x20 && b < 0x7f) || b >= 0x80 {
				printable++
			}
		}
		if float64(printable)/float64(len(sample)) > 0.95 {
			return "text"
		}
	}
	return "unknown"
}

// allowedDocType enforces the allowlist: extension must be supported AND the
// sniffed content kind must match the extension's kind (a video renamed
// claim.pdf fails; a real PDF named scan1.pdf passes).
func allowedDocType(lowerName string, head []byte) error {
	kind, ok := allowedDocExts[filepath.Ext(lowerName)]
	if !ok {
		return fmt.Errorf("file type not accepted — upload PDF, Word/Excel/PowerPoint, image (PNG/JPEG/TIFF), or text/CSV documents")
	}
	if len(head) == 0 {
		return nil // extension-only check (multipart initiation); magic verified on first chunk
	}
	sniffed := sniffKind(head)
	if sniffed != kind {
		if sniffed == "unknown" {
			return fmt.Errorf("file content is not a recognized document type")
		}
		return fmt.Errorf("file content (%s) does not match its extension — possible disguised upload", sniffed)
	}
	return nil
}

// ---- Abuse throttling ------------------------------------------------------------

// rateLimit allows `limit` requests per window per key, backed by Redis.
// Fail-open for authenticated routes would be fine; for anonymous ShareBox
// routes we fail-closed at the caller's discretion.
func (s *server) rateLimit(key string, limit int64, windowSec int) bool {
	res, err := s.rds.cmd("INCR", "idre:rl:"+key)
	if err != nil {
		return false // Redis down: treat as limited for public surfaces
	}
	var n int64
	fmt.Sscanf(res, "%d", &n)
	if n == 1 {
		_, _ = s.rds.cmd("EXPIRE", "idre:rl:"+key, fmt.Sprintf("%d", windowSec))
	}
	return n <= limit
}

// shareThrottle guards every anonymous ShareBox request: per-IP and per-token
// ceilings keep link spraying / upload floods from becoming a DoS.
func (s *server) shareThrottle(w http.ResponseWriter, r *http.Request, token string) bool {
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.Split(fwd, ",")[0]
	}
	if !s.rateLimit("ip:"+ip, 60, 60) { // 60 req/min per IP
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		return false
	}
	if !s.rateLimit("tok:"+token, 120, 60) { // 120 req/min per token (chunked uploads need headroom)
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
		return false
	}
	return true
}

// checkLinkBudgets enforces per-link file count and cumulative byte budgets.
func (s *server) checkLinkBudgets(w http.ResponseWriter, r *http.Request, token string, addBytes int64) bool {
	var filesUsed, maxFiles int
	var bytesUsed, maxBytes int64
	err := s.db.QueryRow(r.Context(), `
		SELECT files_used, max_files, bytes_used, max_bytes FROM public.share_links WHERE token=$1`,
		token).Scan(&filesUsed, &maxFiles, &bytesUsed, &maxBytes)
	if err != nil {
		http.Error(w, `{"error":"link invalid"}`, http.StatusGone)
		return false
	}
	if filesUsed >= maxFiles {
		http.Error(w, `{"error":"this link has reached its file limit"}`, http.StatusForbidden)
		return false
	}
	if bytesUsed+addBytes > maxBytes {
		http.Error(w, `{"error":"this link has reached its total size limit"}`, http.StatusForbidden)
		return false
	}
	return true
}

func (s *server) bumpLinkUsage(r *http.Request, token string, bytes int64) {
	_, _ = s.db.Exec(r.Context(), `
		UPDATE public.share_links SET files_used=files_used+1, bytes_used=bytes_used+$2 WHERE token=$1`,
		token, bytes)
}
