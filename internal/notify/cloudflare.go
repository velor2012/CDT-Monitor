package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// CloudflareClient updates DNS A records through the Cloudflare v4 API.
type CloudflareClient struct {
	httpClient *http.Client
}

func NewCloudflareClient() *CloudflareClient {
	return &CloudflareClient{httpClient: &http.Client{Timeout: 15 * time.Second}}
}

type cfRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// SyncARecord creates or updates the A record for name so it points at ip.
// zoneID may be empty; it is then resolved from rootDomain.
func (c *CloudflareClient) SyncARecord(ctx context.Context, token, zoneID, rootDomain, name, ip string, proxied bool) error {
	if !isPublicIPv4(ip) {
		return fmt.Errorf("公网 IP 无效或非公网 IPv4: %q", ip)
	}
	if zoneID == "" {
		resolved, err := c.resolveZoneID(ctx, token, rootDomain)
		if err != nil {
			return err
		}
		zoneID = resolved
	}
	existing, err := c.findRecord(ctx, token, zoneID, name)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"type":    "A",
		"name":    name,
		"content": ip,
		"ttl":     1,
		"proxied": proxied,
		"comment": "Managed by CDT Monitor",
	}
	method, path := http.MethodPost, "/dns_records"
	if existing != nil {
		method, path = http.MethodPut, "/dns_records/"+existing.ID
	}
	var result struct {
		Success bool           `json:"success"`
		Errors  []cfErrorEntry `json:"errors"`
	}
	if err = c.request(ctx, token, zoneID, method, path, payload, &result); err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("Cloudflare 更新记录失败: %s", formatCFErrors(result.Errors))
	}
	return nil
}

func (c *CloudflareClient) findRecord(ctx context.Context, token, zoneID, name string) (*cfRecord, error) {
	var result struct {
		Success bool           `json:"success"`
		Errors  []cfErrorEntry `json:"errors"`
		Result  []cfRecord     `json:"result"`
	}
	path := "/dns_records?type=A&name=" + urlQueryEscape(name)
	if err := c.request(ctx, token, zoneID, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	if !result.Success {
		return nil, fmt.Errorf("Cloudflare 查询记录失败: %s", formatCFErrors(result.Errors))
	}
	if len(result.Result) == 0 {
		return nil, nil
	}
	return &result.Result[0], nil
}

// TestConnection validates the pool's DDNS configuration with read-only API
// calls: token validity, zone resolution, and current A-record target.
func (c *CloudflareClient) TestConnection(ctx context.Context, token, zoneID, rootDomain, name string) (map[string]string, error) {
	details := map[string]string{}
	if token == "" {
		return details, errors.New("Cloudflare API Token 未配置")
	}
	resolvedZone := zoneID
	if resolvedZone == "" {
		var err error
		resolvedZone, err = c.resolveZoneID(ctx, token, rootDomain)
		if err != nil {
			details["zone"] = "解析失败"
			return details, err
		}
		details["zone"] = resolvedZone + "（按域名解析）"
	} else {
		// Verify the configured zone ID is accessible with this token.
		var zoneResult struct {
			Success bool           `json:"success"`
			Errors  []cfErrorEntry `json:"errors"`
			Result  []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"result"`
		}
		if err := c.request(ctx, token, resolvedZone, http.MethodGet, "", nil, &zoneResult); err != nil {
			details["zone"] = "查询失败"
			return details, err
		}
		if !zoneResult.Success {
			details["zone"] = "不可访问"
			return details, fmt.Errorf("Zone ID 不可访问: %s", formatCFErrors(zoneResult.Errors))
		}
		if len(zoneResult.Result) == 0 {
			details["zone"] = "不存在"
			return details, errors.New("Zone ID 不存在或 Token 无权限")
		}
		details["zone"] = resolvedZone
	}
	record, err := c.findRecord(ctx, token, resolvedZone, name)
	if err != nil {
		details["record"] = "查询失败"
		return details, err
	}
	if record == nil {
		details["record"] = "尚无 A 记录（首次轮转时将创建）"
	} else {
		details["record"] = record.Content
	}
	return details, nil
}

func (c *CloudflareClient) resolveZoneID(ctx context.Context, token, rootDomain string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(rootDomain))
	domain = strings.Trim(strings.Split(strings.TrimPrefix(strings.TrimPrefix(domain, "https://"), "http://"), "/")[0], ".")
	if domain == "" || !strings.Contains(domain, ".") {
		return "", fmt.Errorf("根域名无效: %q", rootDomain)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.cloudflare.com/client/v4/zones?name="+urlQueryEscape(domain)+"&status=active", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Cloudflare Zone 查询失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var result struct {
		Success bool           `json:"success"`
		Errors  []cfErrorEntry `json:"errors"`
		Result  []struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err = json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("Cloudflare Zone 响应解析失败: %w", err)
	}
	if !result.Success {
		return "", fmt.Errorf("Cloudflare Zone 查询失败: %s", formatCFErrors(result.Errors))
	}
	if len(result.Result) == 0 || result.Result[0].ID == "" {
		return "", fmt.Errorf("Cloudflare 未找到域名 %s 的 Zone，请确认 Token 权限或手动填写 Zone ID", domain)
	}
	return result.Result[0].ID, nil
}

func (c *CloudflareClient) request(ctx context.Context, token, zoneID, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4/zones/"+zoneID+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Cloudflare 网络请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("Cloudflare 响应解析失败（状态码 %d）: %w", resp.StatusCode, err)
	}
	return nil
}

type cfErrorEntry struct {
	Message string `json:"message"`
}

func formatCFErrors(errors []cfErrorEntry) string {
	if len(errors) == 0 {
		return "未知错误"
	}
	messages := make([]string, 0, len(errors))
	for _, entry := range errors {
		if entry.Message == "" {
			messages = append(messages, "未知错误")
			continue
		}
		messages = append(messages, entry.Message)
	}
	return strings.Join(messages, "；")
}

func isPublicIPv4(ip string) bool {
	parsed, err := netip.ParseAddr(ip)
	if err != nil || !parsed.Is4() {
		return false
	}
	return parsed.IsGlobalUnicast() && !parsed.IsPrivate() && !parsed.IsLoopback() && !parsed.IsLinkLocalUnicast() && !parsed.IsMulticast()
}


func urlQueryEscape(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == '~' {
			builder.WriteRune(char)
			continue
		}
		builder.WriteString(fmt.Sprintf("%%%02X", char))
	}
	return builder.String()
}
