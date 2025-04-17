package main

import (
    "bufio"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "net/url"
    "os"
    "sort"
    "strconv"
    "strings"
    "sync"
    "time"
)

type ProxyFetcher struct {
    proxies sync.Map
    sources []string
}

type GeonodeResponse struct {
    Data []struct {
        IP   string `json:"ip"`
        Port string `json:"port"`
    } `json:"data"`
}

func NewProxyFetcher() *ProxyFetcher {
    return &ProxyFetcher{
        sources: []string{
            "https://proxylist.geonode.com/api/proxy-list?limit=500&page=1&sort_by=lastChecked&sort_type=desc&protocols=http%2Chttps",
            "https://www.proxy-list.download/api/v1/get?type=http",
            "https://www.proxy-list.download/api/v1/get?type=https",
        },
    }
}

func (pf *ProxyFetcher) fetchURL(url string) (string, error) {
    client := &http.Client{Timeout: 15 * time.Second}
    req, err := http.NewRequest("GET", url, nil)
    if err != nil {
        return "", err
    }

    req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")

    resp, err := client.Do(req)
    if err != nil {
        log.Printf("Error fetching %s: %v", url, err)
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        log.Printf("Failed to fetch %s: Status %d", url, resp.StatusCode)
        return "", fmt.Errorf("status code: %d", resp.StatusCode)
    }

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return "", err
    }

    return string(body), nil
}

func (pf *ProxyFetcher) parseProxyList(content, url string) {
    if content == "" {
        return
    }

    if strings.Contains(url, "api") && strings.Contains(url, "geonode") {
        var data GeonodeResponse
        if err := json.Unmarshal([]byte(content), &data); err != nil {
            log.Printf("Error parsing JSON from %s: %v", url, err)
            return
        }

        for _, item := range data.Data {
            proxy := fmt.Sprintf("%s:%s", item.IP, item.Port)
            pf.proxies.Store(proxy, true)
        }
        return
    }

    scanner := bufio.NewScanner(strings.NewReader(content))
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if line == "" || !strings.Contains(line, ":") {
            continue
        }

        parts := strings.Fields(line)
        proxy := parts[0]
        hostPort := strings.Split(proxy, ":")
        if len(hostPort) < 2 {
            continue
        }

        host, port := hostPort[0], hostPort[1]
        if portNum, err := strconv.Atoi(port); err == nil {
            if portNum >= 1 && portNum <= 65535 {
                pf.proxies.Store(fmt.Sprintf("%s:%s", host, port), true)
            }
        }
    }
}

func (pf *ProxyFetcher) fetchAllProxies() {
    var wg sync.WaitGroup
    results := make(chan struct {
        url     string
        content string
    }, len(pf.sources))

    for _, url := range pf.sources {
        wg.Add(1)
        go func(url string) {
            defer wg.Done()
            content, err := pf.fetchURL(url)
            if err == nil {
                results <- struct {
                    url     string
                    content string
                }{url, content}
            }
        }(url)
    }

    go func() {
        wg.Wait()
        close(results)
    }()

    for result := range results {
        pf.parseProxyList(result.content, result.url)
    }
}

func (pf *ProxyFetcher) checkProxy(proxy string) (bool, time.Duration) {
    proxyURL, err := url.Parse(fmt.Sprintf("http://%s", proxy))
    if err != nil {
        log.Printf("Invalid proxy URL %s: %v", proxy, err)
        return false, 0
    }

    transport := &http.Transport{
        Proxy: http.ProxyURL(proxyURL),
    }

    client := &http.Client{
        Transport: transport,
        Timeout:   10 * time.Second,
    }

    start := time.Now()
    resp, err := client.Get("http://www.google.com")
    if err != nil {
        log.Printf("Proxy %s failed: %v", proxy, err)
        return false, 0
    }
    defer resp.Body.Close()

    latency := time.Since(start)
    if resp.StatusCode != http.StatusOK {
        log.Printf("Proxy %s returned non-200 status: %d", proxy, resp.StatusCode)
        return false, 0
    }

    if latency > 5*time.Second {
        log.Printf("Proxy %s too slow: %v", proxy, latency)
        return false, latency
    }

    log.Printf("Proxy %s is valid with latency: %v", proxy, latency)
    return true, latency
}

func (pf *ProxyFetcher) checkAndFilterProxies() []string {
    var validProxies []string
    var wg sync.WaitGroup
    results := make(chan struct {
        proxy   string
        valid   bool
        latency time.Duration
    })

    pf.proxies.Range(func(key, _ interface{}) bool {
        wg.Add(1)
        go func(proxy string) {
            defer wg.Done()
            valid, latency := pf.checkProxy(proxy)
            results <- struct {
                proxy   string
                valid   bool
                latency time.Duration
            }{proxy, valid, latency}
        }(key.(string))
        return true
    })

    go func() {
        wg.Wait()
        close(results)
    }()

    for result := range results {
        if result.valid {
            validProxies = append(validProxies, result.proxy)
        }
    }

    return validProxies
}

// sendToTelegram sends the proxy list to a Telegram channel in proxychains format
func (pf *ProxyFetcher) sendToTelegram(proxies []string) error {
    botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
    chatID := os.Getenv("TELEGRAM_CHANNEL_ID")

    if botToken == "" || chatID == "" {
        return fmt.Errorf("TELEGRAM_BOT_TOKEN or TELEGRAM_CHANNEL_ID not set")
    }

    if len(proxies) == 0 {
        return fmt.Errorf("no proxies to send")
    }

    // Prepare message in proxychains format
    timestamp := time.Now().Format("2006-01-02 15:04:05")
    header := fmt.Sprintf("# Proxychains Proxy List - Updated: %s\n# Total working proxies: %d\n# Sources used: %d\n\n", timestamp, len(proxies), len(pf.sources))
    var proxyLines []string
    for _, proxy := range proxies {
        parts := strings.Split(proxy, ":")
        if len(parts) == 2 {
            proxyLines = append(proxyLines, fmt.Sprintf("http %s %s", parts[0], parts[1]))
        }
    }
    proxyList := strings.Join(proxyLines, "\n")
    // Wrap in Markdown code block for monospace
    message := fmt.Sprintf("```\n%s%s\n```", header, proxyList)

    // Telegram message size limit is 4096 characters; split if necessary
    const maxMessageSize = 4096
    if len(message) <= maxMessageSize {
        return sendTelegramMessage(botToken, chatID, message)
    }

    // Split into multiple messages
    var messages []string
    current := "```\n" + header
    for _, line := range proxyLines {
        nextLine := line + "\n"
        if len(current)+len(nextLine)+3 > maxMessageSize { // +3 for closing ```
            current += "```"
            messages = append(messages, current)
            current = "```\n" + header
        }
        current += nextLine
    }
    if len(current) > len("```\n"+header) {
        current += "```"
        messages = append(messages, current)
    }

    // Send each message
    for i, msg := range messages {
        if err := sendTelegramMessage(botToken, chatID, msg); err != nil {
            log.Printf("Failed to send message %d to Telegram: %v", i+1, err)
            return err
        }
    }

    log.Printf("Sent %d proxies to Telegram channel %s", len(proxies), chatID)
    return nil
}

// sendTelegramMessage sends a single message to Telegram with Markdown parsing
func sendTelegramMessage(botToken, chatID, message string) error {
    apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
    data := url.Values{
        "chat_id":    {chatID},
        "text":       {message},
        "parse_mode": {"MarkdownV2"}, // Enable Markdown formatting
    }

    resp, err := http.PostForm(apiURL, data)
    if err != nil {
        return fmt.Errorf("failed to send Telegram message: %v", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("Telegram API error: status %d, response: %s", resp.StatusCode, string(body))
    }

    return nil
}

func (pf *ProxyFetcher) saveProxies() {
    proxies := pf.checkAndFilterProxies()

    if len(proxies) == 0 {
        log.Println("No working proxies found to save!")
        return
    }

    // Sort proxies by IP and port
    sort.Slice(proxies, func(i, j int) bool {
        pi, pj := proxies[i], proxies[j]
        partsI := strings.Split(pi, ":")
        partsJ := strings.Split(pj, ":")
        ipPartsI := strings.Split(partsI[0], ".")
        ipPartsJ := strings.Split(partsJ[0], ".")

        for k := 0; k < 4; k++ {
            numI, _ := strconv.Atoi(ipPartsI[k])
            numJ, _ := strconv.Atoi(ipPartsJ[k])
            if numI != numJ {
                return numI < numJ
            }
        }

        portI, _ := strconv.Atoi(partsI[1])
        portJ, _ := strconv.Atoi(partsJ[1])
        return portI < portJ
    })

    // Save to proxychains.conf
    file, err := os.Create("proxychains.conf")
    if err != nil {
        log.Printf("Error creating proxychains.conf: %v", err)
    } else {
        defer file.Close()
        timestamp := time.Now().Format("2006-01-02 15:04:05")
        fmt.Fprintf(file, "# Proxychains configuration - Updated: %s\n", timestamp)
        fmt.Fprintf(file, "# Total working proxies: %d\n", len(proxies))
        fmt.Fprintf(file, "# Sources used: %d\n", len(pf.sources))
        fmt.Fprintf(file, "# Format: http < BeethovenIP> <port>\n\n")

        for _, proxy := range proxies {
            parts := strings.Split(proxy, ":")
            if len(parts) == 2 {
                fmt.Fprintf(file, "http %s %s\n", parts[0], parts[1])
            }
        }
        log.Printf("Saved %d working proxies to proxychains.conf", len(proxies))
    }

    // Save to proxies.txt
    file, err = os.Create("proxies.txt")
    if err != nil {
        log.Printf("Error creating proxies.txt: %v", err)
    } else {
        defer file.Close()
        timestamp := time.Now().Format("2006-01-02 15:04:05")
        fmt.Fprintf(file, "# Proxy List - Updated: %s\n", timestamp)
        fmt.Fprintf(file, "# Total working proxies: %d\n", len(proxies))
        fmt.Fprintf(file, "# Sources used: %d\n\n", len(pf.sources))

        for _, proxy := range proxies {
            fmt.Fprintf(file, "%s\n", proxy)
        }
        log.Printf("Saved %d working proxies to proxies.txt", len(proxies))
    }

    // Send to Telegram
    if err := pf.sendToTelegram(proxies); err != nil {
        log.Printf("Error sending proxies to Telegram: %v", err)
    }
}

func main() {
    fetcher := NewProxyFetcher()
    fetcher.fetchAllProxies()
    fetcher.saveProxies()
}
