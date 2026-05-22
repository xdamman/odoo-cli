package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// rpcResponse is the JSON-RPC envelope Odoo returns.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Debug string `json:"debug"`
		} `json:"data"`
	} `json:"error"`
}

const (
	rpcMaxRetries     = 5
	rpcRetryBaseDelay = 2 * time.Second
)

// RPC is the low-level JSON-RPC call. Most callers want Exec.
//
// Handles HTTP 429 (Odoo SaaS rate-limit) with exponential backoff
// honouring Retry-After, and converts common transport / RPC errors
// into actionable messages (DNS, TLS, deleted-instance redirect,
// "database does not exist", wrong credentials, …).
func RPC(odooURL, service, method string, args []interface{}) (json.RawMessage, error) {
	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "call",
		"params": map[string]interface{}{
			"service": service,
			"method":  method,
			"args":    args,
		},
		"id": time.Now().UnixNano(),
	}
	data, _ := json.Marshal(payload)

	delay := rpcRetryBaseDelay
	for attempt := 0; ; attempt++ {
		resp, err := http.Post(odooURL+"/jsonrpc", "application/json", bytes.NewReader(data))
		if err != nil {
			return nil, friendlyNetworkError(odooURL, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 429 && attempt < rpcMaxRetries {
			wait := delay
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := time.ParseDuration(ra + "s"); err == nil && secs > 0 {
					wait = secs
				}
			}
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			time.Sleep(wait)
			delay *= 2
			continue
		}

		var rr rpcResponse
		if err := json.Unmarshal(body, &rr); err != nil {
			if resp.StatusCode == 429 {
				return nil, fmt.Errorf("rate-limited by Odoo (HTTP 429) after %d retries — try again in a minute", attempt)
			}
			return nil, friendlyTransportError(odooURL, resp.StatusCode, body)
		}
		return handleResponse(rr)
	}
}

// Auth authenticates against Odoo and returns the uid.
func Auth(odooURL, db, login, password string) (int, error) {
	result, err := RPC(odooURL, "common", "authenticate", []interface{}{
		db, login, password, map[string]interface{}{},
	})
	if err != nil {
		return 0, err
	}
	var uid int
	if err := json.Unmarshal(result, &uid); err != nil || uid == 0 {
		return 0, fmt.Errorf("Odoo rejected the credentials — check login + password (login=%s, db=%s)", login, db)
	}
	return uid, nil
}

// Exec calls `execute_kw` on the given model + method. The standard
// shape for nearly every Odoo RPC.
func Exec(odooURL, db string, uid int, password, model, method string, args []interface{}, kwargs map[string]interface{}) (json.RawMessage, error) {
	callArgs := []interface{}{db, uid, password, model, method, args}
	if kwargs == nil {
		kwargs = map[string]interface{}{}
	}
	callArgs = append(callArgs, kwargs)
	return RPC(odooURL, "object", "execute_kw", callArgs)
}

// SearchReadAllMaps paginates a search_read until every row has been
// fetched. Returns the rows as []map[string]interface{}.
//
//	domain   — odoo search domain, e.g. [["state","=","posted"]]
//	fields   — field names to return
//	order    — sort order ("id asc", "date desc", …) or "" for default
func SearchReadAllMaps(db *Database, uid int, model string, domain []interface{}, fields []string, order string) ([]map[string]interface{}, error) {
	const pageSize = 200
	out := make([]map[string]interface{}, 0)
	offset := 0
	for {
		kwargs := map[string]interface{}{
			"fields": fields,
			"limit":  pageSize,
			"offset": offset,
		}
		if order != "" {
			kwargs["order"] = order
		}
		raw, err := Exec(db.URL, db.DB, uid, db.Password, model, "search_read",
			[]interface{}{domain}, kwargs)
		if err != nil {
			return nil, err
		}
		var page []map[string]interface{}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("unmarshal %s page: %w", model, err)
		}
		out = append(out, page...)
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	return out, nil
}

// AuthDatabase is a convenience wrapper that resolves the Database
// from the env file, runs Auth, and returns both. Every command
// that touches Odoo calls this once at the top of its flow.
func AuthDatabase(db *Database) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("nil database")
	}
	uid, err := Auth(db.URL, db.DB, db.Login, db.Password)
	if err != nil {
		return 0, err
	}
	if uid == 0 {
		return 0, fmt.Errorf("Odoo returned uid=0 (auth failed silently)")
	}
	return uid, nil
}

// ── Friendly error formatting ────────────────────────────────────

func friendlyNetworkError(odooURL string, err error) error {
	host := odooURL
	if u, perr := url.Parse(odooURL); perr == nil && u.Host != "" {
		host = u.Host
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "dns"):
		return fmt.Errorf("Odoo host %s cannot be resolved (DNS lookup failed). Check ODOO_URL in the .env file", host)
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("Odoo at %s refused the connection — the service may be down", host)
	case strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "timeout"):
		return fmt.Errorf("Odoo at %s timed out — the instance is slow or unreachable; try again", host)
	case strings.Contains(msg, "x509"), strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"):
		return fmt.Errorf("Odoo at %s has a TLS/certificate problem: %v", host, err)
	}
	return fmt.Errorf("could not reach Odoo at %s: %v", host, err)
}

func friendlyTransportError(odooURL string, status int, body []byte) error {
	host := odooURL
	if u, err := url.Parse(odooURL); err == nil && u.Host != "" {
		host = u.Host
	}
	preview := strings.ToLower(string(body))
	if status == 404 && strings.Contains(preview, "odoo.com/typo") {
		return fmt.Errorf("Odoo instance %s does not exist (or has been removed). Check ODOO_URL", host)
	}
	if status == 404 {
		return fmt.Errorf("Odoo URL %s returned 404 — the JSON-RPC endpoint is not reachable. Verify ODOO_URL points at a live Odoo instance", odooURL)
	}
	if strings.Contains(preview, "<title>odoo") || strings.Contains(preview, "name=\"login\"") {
		return fmt.Errorf("Odoo at %s returned an HTML login page instead of JSON-RPC — the database name is likely wrong (set ODOO_DATABASE)", host)
	}
	if status >= 500 {
		return fmt.Errorf("Odoo at %s is unhealthy (HTTP %d) — try again in a moment", host, status)
	}
	snippet := strings.Join(strings.Fields(string(body)), " ")
	if len(snippet) > 160 {
		snippet = snippet[:160] + "…"
	}
	return fmt.Errorf("Odoo at %s returned an unexpected response (HTTP %d): %s", host, status, snippet)
}

func handleResponse(rr rpcResponse) (json.RawMessage, error) {
	if rr.Error != nil {
		msg := rr.Error.Message
		if rr.Error.Data.Debug != "" {
			lines := strings.Split(rr.Error.Data.Debug, "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				if s := strings.TrimSpace(lines[i]); s != "" {
					msg = s
					break
				}
			}
		}
		if msg == "" {
			msg = "(empty error response)"
		}
		return nil, friendlyRPCError(msg)
	}
	return rr.Result, nil
}

func friendlyRPCError(msg string) error {
	low := strings.ToLower(msg)
	if strings.Contains(low, "database") && strings.Contains(low, "does not exist") {
		if m := dbNameInError.FindStringSubmatch(msg); len(m) > 1 {
			return fmt.Errorf("Odoo database %q does not exist on this server. Check ODOO_DATABASE", m[1])
		}
		return fmt.Errorf("Odoo database does not exist on this server. Check ODOO_DATABASE")
	}
	if strings.Contains(low, "access denied") || strings.Contains(low, "accessdenied") ||
		strings.Contains(low, "invalid login") || strings.Contains(low, "wrong login/password") {
		return fmt.Errorf("Odoo rejected the credentials — check ODOO_LOGIN and ODOO_PASSWORD")
	}
	return fmt.Errorf("odoo error: %s", msg)
}

var dbNameInError = regexp.MustCompile(`database\s+"([^"]+)"\s+does not exist`)

// ── Field extractors for Odoo's many2one / one2many response shapes ──

// FieldID extracts the integer id from a many2one response, which
// comes back as [id, name] or false.
func FieldID(v interface{}) int {
	switch x := v.(type) {
	case []interface{}:
		if len(x) > 0 {
			return Int(x[0])
		}
	case float64:
		return int(x)
	}
	return 0
}

// FieldName extracts the display name from a many2one response.
func FieldName(v interface{}) string {
	if arr, ok := v.([]interface{}); ok && len(arr) > 1 {
		if s, ok := arr[1].(string); ok {
			return s
		}
	}
	return ""
}

// Int returns the numeric value at v, accepting either float64 or int.
func Int(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

// Float returns the numeric value at v, accepting float64 / int.
func Float(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

// Str returns the string value at v, or "" when v isn't a string.
func Str(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// Bool returns the boolean value at v. Odoo emits literal false for
// "missing" relations, which we treat as zero values everywhere.
func Bool(v interface{}) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
