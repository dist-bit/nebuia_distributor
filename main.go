package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"ocr/src/terminal"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	CheckNewDocsInterval = 10 // Seconds between each search for new documents
	CursorUp             = "\033[A"
	CursorDown           = "\033[B"
	ClearLine            = "\033[K"
)

// ServerStats holds statistics for each server
type ServerStats struct {
	processed    int
	capacityFull int
}

type ServerHealth struct {
	URL       string
	Healthy   bool
	LastCheck time.Time
	Lock      sync.RWMutex
}

// ProcessingResult represents the result of a document processing attempt
type ProcessingResult struct {
	UUID      string    `json:"uuid"`
	ServerURL string    `json:"server_url"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Success   bool      `json:"success"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	Attempts  int       `json:"attempts"`
}

func (p ProcessingResult) Duration() float64 {
	return p.EndTime.Sub(p.StartTime).Seconds()
}

// Document represents a PDF document to be processed
type Document struct {
	UUID     string `json:"uuid" bson:"uuid"`
	URL      string `json:"url" bson:"url"`
	FileName string `json:"file_name" bson:"file_name"`
}

// StatusUpdate represents a status update for the live tracker
type StatusUpdate struct {
	DocID    string
	Status   string
	Details  string
	Category string
}

// LiveDocumentTracker handles real-time display of document processing status
type LiveDocumentTracker struct {
	documentLines map[string]int
	systemLines   map[string]int
	nextLine      int
	lock          sync.Mutex
	statusChan    chan StatusUpdate
	isRunning     bool
	completedDocs map[string]time.Time
	webTerminal   *terminal.WebTerminal
}

func NewLiveDocumentTracker() *LiveDocumentTracker {
	return &LiveDocumentTracker{
		documentLines: make(map[string]int),
		systemLines:   make(map[string]int),
		statusChan:    make(chan StatusUpdate, 100),
		completedDocs: make(map[string]time.Time),
		isRunning:     true,
	}
}

func (t *LiveDocumentTracker) Start() {
	clearScreen()
	fmt.Print(strings.Repeat("\n", 40))
	fmt.Print(strings.Repeat(CursorUp, 40))

	go t.updateDisplay()
	go t.cleanupCompleted()
}

func (t *LiveDocumentTracker) cleanupCompleted() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for t.isRunning {
		select {
		case <-ticker.C:
			currentTime := time.Now()
			t.lock.Lock()
			for docID, completionTime := range t.completedDocs {
				if currentTime.Sub(completionTime) >= 3*time.Second {
					if lineNum, exists := t.documentLines[docID]; exists {
						fmt.Print(strings.Repeat(CursorUp, t.nextLine-lineNum))
						fmt.Print(ClearLine)

						for otherID, otherLine := range t.documentLines {
							if otherLine > lineNum {
								t.documentLines[otherID] = otherLine - 1
							}
						}

						t.nextLine--
						fmt.Print(strings.Repeat(CursorDown, t.nextLine-lineNum))

						delete(t.documentLines, docID)
						delete(t.completedDocs, docID)
					}
				}
			}
			t.lock.Unlock()
		}
	}
}

func (t *LiveDocumentTracker) getStatusColor(status string) *color.Color {
	switch status {
	case "WAITING":
		return color.New(color.FgCyan)
	case "PROCESSING":
		return color.New(color.FgYellow)
	case "REJECTED":
		return color.New(color.FgMagenta)
	case "SUCCESS":
		return color.New(color.FgGreen)
	case "ERROR":
		return color.New(color.FgRed)
	default:
		return color.New(color.FgWhite)
	}
}

func (t *LiveDocumentTracker) getStatusSymbol(status string) string {
	switch status {
	case "WAITING":
		return "⌛"
	case "PROCESSING":
		return "⚙️"
	case "REJECTED":
		return "⚠️"
	case "SUCCESS":
		return "✅"
	case "ERROR":
		return "❌"
	default:
		return "•"
	}
}

func (t *LiveDocumentTracker) updateDisplay() {
	for update := range t.statusChan {
		t.lock.Lock()

		var lineNum int
		isNewLine := false

		if update.Category != "" {
			// For system and stats messages, reuse existing line if possible
			if existingLine, exists := t.systemLines[update.Category]; exists {
				lineNum = existingLine
			} else {
				lineNum = t.nextLine
				t.nextLine++
				t.systemLines[update.Category] = lineNum
				isNewLine = true
			}
		} else {
			// For document processing messages, keep existing behavior
			if _, exists := t.documentLines[update.DocID]; !exists {
				lineNum = t.nextLine
				t.nextLine++
				t.documentLines[update.DocID] = lineNum
				isNewLine = true
			} else {
				lineNum = t.documentLines[update.DocID]
			}
		}

		statusColor := t.getStatusColor(update.Status)
		symbol := t.getStatusSymbol(update.Status)

		// Move cursor to line position
		if !isNewLine {
			fmt.Print(strings.Repeat(CursorUp, t.nextLine-lineNum))
		}
		fmt.Print(ClearLine)
		statusColor.Printf("%s", update.DocID)
		fmt.Printf(" %s ", symbol)
		statusColor.Printf("%-10s", update.Status)
		fmt.Printf("| %s\n", update.Details)
		if !isNewLine {
			fmt.Print(strings.Repeat(CursorDown, t.nextLine-lineNum-1))
		}

		if update.Status == "SUCCESS" && update.Category == "" {
			t.completedDocs[update.DocID] = time.Now()
		}
		t.lock.Unlock()
	}
}

func (t *LiveDocumentTracker) UpdateStatus(docID, status, details, category string) {
	t.statusChan <- StatusUpdate{DocID: docID, Status: status, Details: details, Category: category}
	if t.webTerminal != nil {
		symbol := t.getStatusSymbol(status)
		line := fmt.Sprintf("%s %s %-10s| %s\n", docID, symbol, status, details)
		t.webTerminal.WriteLine(line)
	}
}

func (t *LiveDocumentTracker) Stop() {
	t.isRunning = false
	close(t.statusChan)
}

type PDFProcessorClient struct {
	servers            []string
	timeout            time.Duration
	pollInterval       time.Duration
	pendingDocuments   map[string]Document
	processLock        sync.Mutex
	isRunning          bool
	serverStats        map[string]ServerStats
	documentAttempts   map[string]int
	tracker            *LiveDocumentTracker
	processedUUIDs     map[string]bool
	mongoClient        *mongo.Client
	serverHealth       map[string]*ServerHealth
	currentServerIndex int
	serverMutex        sync.Mutex
}

func NewPDFProcessorClient(servers []string, timeout, pollInterval time.Duration) *PDFProcessorClient {
	serverStats := make(map[string]ServerStats)
	serverHealth := make(map[string]*ServerHealth)

	for _, server := range servers {
		serverStats[server] = ServerStats{0, 0}
		serverHealth[server] = &ServerHealth{
			URL:       server,
			Healthy:   true,
			LastCheck: time.Time{},
		}
	}

	return &PDFProcessorClient{
		servers:            servers,
		timeout:            timeout,
		pollInterval:       pollInterval,
		pendingDocuments:   make(map[string]Document),
		serverStats:        serverStats,
		serverHealth:       serverHealth,
		documentAttempts:   make(map[string]int),
		tracker:            NewLiveDocumentTracker(),
		processedUUIDs:     make(map[string]bool),
		isRunning:          true,
		currentServerIndex: 0,
	}
}

func (c *PDFProcessorClient) selectNextServer() string {
	c.serverMutex.Lock()
	defer c.serverMutex.Unlock()

	initialIndex := c.currentServerIndex
	serversCount := len(c.servers)

	for i := 0; i < serversCount; i++ {
		server := c.servers[c.currentServerIndex]
		c.currentServerIndex = (c.currentServerIndex + 1) % serversCount

		if c.checkServerHealth(server) {
			return server
		}
	}

	c.currentServerIndex = initialIndex
	return ""
}

func (c *PDFProcessorClient) checkServerHealth(serverURL string) bool {
	health := c.serverHealth[serverURL]
	health.Lock.RLock()
	if time.Since(health.LastCheck) < 30*time.Second {
		isHealthy := health.Healthy
		health.Lock.RUnlock()
		return isHealthy
	}
	health.Lock.RUnlock()

	health.Lock.Lock()
	defer health.Lock.Unlock()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(serverURL + "/health")
	if err != nil {
		health.Healthy = false
		health.LastCheck = time.Now()
		return false
	}
	defer resp.Body.Close()

	health.Healthy = resp.StatusCode == http.StatusOK
	health.LastCheck = time.Now()
	return health.Healthy
}

func (c *PDFProcessorClient) addDocumentsToQueue(documents []Document) {
	if len(documents) == 0 {
		return
	}

	c.processLock.Lock()
	defer c.processLock.Unlock()

	for _, doc := range documents {
		if !c.processedUUIDs[doc.UUID] {
			if _, exists := c.pendingDocuments[doc.UUID]; !exists {
				c.pendingDocuments[doc.UUID] = doc
				c.logStatus(
					doc.UUID,
					"WAITING",
					"New document detected - Added to processing queue",
				)
			}
		}
	}
}

func (c *PDFProcessorClient) tryProcessDocument(doc Document, serverURL string) (bool, bool) {
	c.processLock.Lock()

	if c.processedUUIDs[doc.UUID] {
		c.processLock.Unlock()
		return true, false
	}

	if !c.checkServerHealth(serverURL) {
		c.processLock.Unlock()
		return false, false
	}

	c.documentAttempts[doc.UUID]++
	attemptCount := c.documentAttempts[doc.UUID]
	c.processLock.Unlock()

	serverName := serverURL[strings.LastIndex(serverURL, "/")+1:]

	c.logStatus(
		doc.UUID,
		"PROCESSING",
		fmt.Sprintf("Attempting to process on %s (Attempt %d)", serverName, attemptCount),
	)

	payload := map[string]string{
		"url":       doc.URL,
		"uuid":      doc.UUID,
		"file_name": doc.FileName,
	}

	startTime := time.Now()

	jsonData, err := json.Marshal(payload)
	if err != nil {
		c.logStatus(doc.UUID, "ERROR", fmt.Sprintf("Serialization error: %v", err))
		return false, false
	}

	client := &http.Client{Timeout: c.timeout}
	resp, err := client.Post(serverURL+"/predict", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		c.logStatus(
			doc.UUID,
			"ERROR",
			fmt.Sprintf("Connection error with %s: %v", serverName, err),
		)
		return false, false
	}
	defer resp.Body.Close()

	var response struct {
		Status string `json:"status"`
		Data   struct {
			Message string `json:"message"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		c.logStatus(doc.UUID, "ERROR", fmt.Sprintf("Response decode error: %v", err))
		return false, false
	}

	if response.Status == "error" && response.Data.Message == "Server at maximum capacity" {
		c.processLock.Lock()
		stats := c.serverStats[serverURL]
		stats.capacityFull++
		c.serverStats[serverURL] = stats
		c.processLock.Unlock()

		c.logStatus(
			doc.UUID,
			"INFO",
			fmt.Sprintf("%s at maximum capacity - Will try next server", serverName),
		)
		return false, true // false = no procesado, true = server lleno
	}

	if response.Status == "success" {
		c.processLock.Lock()
		if !c.processedUUIDs[doc.UUID] {
			duration := time.Since(startTime)
			c.processedUUIDs[doc.UUID] = true
			delete(c.pendingDocuments, doc.UUID)
			stats := c.serverStats[serverURL]
			stats.processed++
			c.serverStats[serverURL] = stats
			c.processLock.Unlock()

			go c.updateDocumentStatus(doc.UUID, "working_ocr")
			c.logStatus(
				doc.UUID,
				"SUCCESS",
				fmt.Sprintf("Successfully processed on %s - Time: %.1fs - Attempts: %d",
					serverName, duration.Seconds(), attemptCount),
			)
			return true, false
		}
		c.processLock.Unlock()
		return true, false
	}

	c.logStatus(
		doc.UUID,
		"ERROR",
		fmt.Sprintf("Error on %s: %s", serverName, response.Data.Message),
	)
	return false, false
}

func (c *PDFProcessorClient) processDocumentWithRotation(doc Document) bool {
	serversCount := len(c.servers)
	serversTried := make(map[string]bool)

	for len(serversTried) < serversCount {
		server := c.selectNextServer()
		if server == "" {
			c.logStatus(
				doc.UUID,
				"WAITING",
				"No healthy servers available - Document queued for retry",
			)
			return false
		}

		if serversTried[server] {
			continue
		}

		success, serverFull := c.tryProcessDocument(doc, server)
		serversTried[server] = true

		if success {
			return true
		}

		if serverFull {
			continue
		}

		time.Sleep(c.pollInterval)
	}

	if len(serversTried) == serversCount {
		allFull := true
		for server := range serversTried {
			if stats := c.serverStats[server]; stats.capacityFull == 0 {
				allFull = false
				break
			}
		}

		if allFull {
			c.logStatus(
				doc.UUID,
				"WAITING",
				"All servers at capacity - Document queued for retry",
			)
		} else {
			c.logStatus(
				doc.UUID,
				"ERROR",
				"All servers attempted - Document will be retried later",
			)
		}
	}

	return false
}

func (c *PDFProcessorClient) ProcessContinuously(ctx context.Context) {
	c.tracker.Start()

	c.logStatus("SYSTEM", "INFO", fmt.Sprintf("Starting PDF processor with %d servers", len(c.servers)))

	var wg sync.WaitGroup

	// Document finder goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(CheckNewDocsInterval * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !c.isRunning {
					return
				}

				docs, err := c.getBatchToWork()
				if err != nil {
					continue
				}

				if len(docs) > 0 {
					c.logStatus("SYSTEM", "INFO", fmt.Sprintf("Found %d new documents", len(docs)))

					for _, doc := range docs {
						c.processLock.Lock()
						if !c.processedUUIDs[doc.UUID] {
							c.pendingDocuments[doc.UUID] = doc
							c.documentAttempts[doc.UUID] = 0
							c.logStatus(doc.UUID, "WAITING", "Document queued for processing")
						}
						c.processLock.Unlock()
					}

				}
			}
		}
	}()

	// Document processor goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		processTicker := time.NewTicker(500 * time.Millisecond)
		defer processTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-processTicker.C:
				if !c.isRunning {
					return
				}

				c.processLock.Lock()
				pendingDocs := make([]Document, 0)
				for _, doc := range c.pendingDocuments {
					if !c.processedUUIDs[doc.UUID] {
						pendingDocs = append(pendingDocs, doc)
					}
				}
				c.processLock.Unlock()

				for _, doc := range pendingDocs {
					c.processLock.Lock()
					if c.processedUUIDs[doc.UUID] {
						c.processLock.Unlock()
						continue
					}
					c.processLock.Unlock()

					success := c.processDocumentWithRotation(doc)
					if success {
						c.processLock.Lock()
						delete(c.pendingDocuments, doc.UUID)
						c.processLock.Unlock()
					} else {
						time.Sleep(c.pollInterval)
					}
				}
			}
		}
	}()

	// Stats reporter goroutine - same as before
	wg.Add(1)
	go func() {
		defer wg.Done()
		statsTicker := time.NewTicker(30 * time.Second)
		defer statsTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-statsTicker.C:
				if !c.isRunning {
					return
				}

				c.processLock.Lock()
				totalProcessed := 0
				totalPending := len(c.pendingDocuments)
				totalAtCapacity := 0
				serverStats := make(map[string]ServerStats)

				for server, stats := range c.serverStats {
					totalProcessed += stats.processed
					if stats.capacityFull > 0 {
						totalAtCapacity++
					}
					serverStats[server] = stats
				}
				c.processLock.Unlock()

				c.logStatus(
					"STATS",
					"INFO",
					fmt.Sprintf("System status - Processed: %d, Pending: %d, Servers at capacity: %d/%d",
						totalProcessed, totalPending, totalAtCapacity, len(c.servers)),
				)

				for server, stats := range serverStats {
					serverName := server[strings.LastIndex(server, "/")+1:]
					c.logStatus(
						"STATS",
						"INFO",
						fmt.Sprintf("Server %s - Processed: %d, Capacity reached: %d times",
							serverName, stats.processed, stats.capacityFull),
					)
				}
			}
		}
	}()

	<-ctx.Done()
	c.logStatus("SYSTEM", "INFO", "Shutdown signal received. Stopping processors...")

	c.isRunning = false

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		c.logStatus("SYSTEM", "INFO", "All processors stopped successfully")
	case <-time.After(10 * time.Second):
		c.logStatus("SYSTEM", "WARNING", "Forced shutdown due to timeout")
	}

	c.tracker.Stop()
}

func (c *PDFProcessorClient) updateDocumentStatus(uuid, status string) error {
	collection := c.mongoClient.Database("sigpad").Collection("documents")
	filter := bson.M{"uuid": uuid}
	update := bson.M{"$set": bson.M{"status_document": status}}

	_, err := collection.UpdateOne(context.Background(), filter, update)
	return err
}

func (c *PDFProcessorClient) getBatchToWork() ([]Document, error) {
	collection := c.mongoClient.Database("sigpad").Collection("documents")

	filter := bson.M{
		"status_document": "waiting_process",
		"url":             bson.M{"$exists": true},
	}

	cursor, err := collection.Find(context.Background(), filter)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	var documents []Document
	if err := cursor.All(context.Background(), &documents); err != nil {
		return nil, err
	}

	return documents, nil
}

func (c *PDFProcessorClient) logStatus(id, status, details string) {
	category := ""

	// Determine if this is a system or stats message that should be tracked
	if id == "SYSTEM" || id == "STATS" {
		if strings.Contains(details, "Searching for new documents") {
			category = "search"
		} else if strings.Contains(details, "System status") {
			category = "system_status"
		} else if strings.Contains(details, "Server") && strings.Contains(details, "Processed:") {
			category = "server_" + strings.Split(details, " ")[1]
		} else if strings.Contains(details, "Server") && strings.Contains(details, "unhealthy") {
			category = "health_" + strings.Split(details, " ")[1]
		}
	}

	c.tracker.UpdateStatus(id, status, details, category)
}

func clearScreen() {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd", "/c", "cls")
		cmd.Stdout = os.Stdout
		cmd.Run()
	} else {
		fmt.Print("\033[H\033[2J")
	}
}

func main() {
	webTerminal := terminal.NewWebTerminal()
	webTerminal.Start()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tmpl := template.Must(template.ParseFiles("terminal.html"))
		tmpl.Execute(w, nil)
	})
	http.HandleFunc("/ws", webTerminal.HandleWebSocket)

	srv := &http.Server{
		Addr:    ":8083",
		Handler: http.DefaultServeMux,
	}
	go func() {
		log.Printf("Starting web server on :8083")
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v\n", err)
		}
	}()

	if err := godotenv.Load(".env.processor"); err != nil {
		log.Printf("Error loading .env file: %v\n", err)
		return
	}

	mongoURI := os.Getenv("MONGO_URL")
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Printf("Error connecting to MongoDB: %v\n", err)
		return
	}

	servers := []string{
		"https://ocr4-service.nebuia.com",
		"https://ocr2-service.nebuia.com",
		"https://ocr-service.nebuia.com",
	}

	processor := NewPDFProcessorClient(servers, 30*time.Second, 10*time.Second)
	processor.mongoClient = client
	processor.tracker.webTerminal = webTerminal

	ctx, cancel := context.WithCancel(context.Background())

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	done := make(chan bool, 1)

	go func() {
		<-sigChan
		fmt.Println("\nShutdown requested. Finishing processes...")

		cancel()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v\n", err)
		}

		if err := client.Disconnect(context.Background()); err != nil {
			log.Printf("MongoDB disconnect error: %v\n", err)
		}

		processor.isRunning = false
		webTerminal.Stop()
		done <- true
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		processor.ProcessContinuously(ctx)
	}()

	select {
	case <-done:
		fmt.Println("Waiting for processes to finish...")
		c := make(chan struct{})
		go func() {
			wg.Wait()
			close(c)
		}()

		select {
		case <-c:
			fmt.Println("Graceful shutdown completed")
		case <-time.After(10 * time.Second):
			fmt.Println("Forced shutdown after timeout")
		}
	}

	fmt.Println("Process terminated")
	os.Exit(0)
}
