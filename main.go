package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ------------------------------
// Types and constants
// ------------------------------

const (
	QuestionInterval       = 10 * time.Minute
	MinAnswerWords         = 5
	IdleThreshold          = 3 * time.Minute // a session ends if no event arrives in this interval
	ClientRateLimitMinutes = 2               // client output only once every 2 minutes
	PeriodicLogInterval    = 1 * time.Second // server logs status every 2 minutes
)

var (
	// sample mindfulness questions (in German)
	questions = []string{
		"Ist diese Handlung mit deinen langfristigen Zielen abgestimmt?",
		"Ist dies gerade die wichtigste Aufgabe?",
		"Vermeidest du etwas Wichtigeres?",
		"Wird dies in einem Jahr noch wichtig sein?",
		"Was ist das schlimmste, was passieren könnte, wenn du das jetzt nicht machst?",
	}
)

// UpdateResponse now includes live-active minutes and time until idle.
type UpdateResponse struct {
	ActiveMinutes     int    `json:"active_minutes"`      // minutes accumulated from closed sessions
	LiveActiveMinutes int    `json:"live_active_minutes"` // closed sessions + current session (if active)
	TimeUntilIdle     string `json:"time_until_idle"`     // time remaining until session would close (if active)
	NextQuestionIn    string `json:"next_question_in"`    // as human-readable
	Message           string `json:"message,omitempty"`
}

type QuestionResponse struct {
	Question string `json:"question"`
}

type InteractionRequest struct {
	Timestamp int64  `json:"timestamp"`
	Folder    string `json:"folder"`
	Question  string `json:"question"`
	Answer    string `json:"answer"`
}

// ------------------------------
// Server struct
// ------------------------------

type Server struct {
	logFilePath string
	statsDir    string

	logMutex   sync.Mutex
	statsMutex sync.Mutex

	// total active time accumulated from closed sessions (as a Duration)
	totalActive time.Duration

	// current active session (if any). When a file event comes in:
	// - If no session is active, one is started.
	// - If one is active and the gap is <= IdleThreshold, its end time is updated.
	// - Otherwise, the old session is closed and a new session is started.
	currentSessionStart time.Time
	currentSessionEnd   time.Time

	// lastQuestionTime is kept in-memory.
	lastQuestionTime time.Time

	// watcher and watched directories.
	watcher     *fsnotify.Watcher
	directories []string

	// current date string "YYYY-MM-DD"
	currentDate string
}

// NewServer creates a new server given the directories to watch.
func NewServer(watchDirs []string) (*Server, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	adhdDir := filepath.Join(homeDir, ".adhd")
	if err := os.MkdirAll(adhdDir, 0755); err != nil {
		return nil, err
	}

	logFilePath := filepath.Join(adhdDir, "log.csv")
	// Create the log file with header if it does not exist.
	if _, err := os.Stat(logFilePath); errors.Is(err, os.ErrNotExist) {
		f, err := os.Create(logFilePath)
		if err != nil {
			return nil, err
		}
		_, err = f.WriteString("timestamp,folder,question,answer\n")
		f.Close()
		if err != nil {
			return nil, err
		}
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	s := &Server{
		logFilePath:      logFilePath,
		statsDir:         adhdDir,
		watcher:          watcher,
		directories:      watchDirs,
		currentDate:      time.Now().Format("2006-01-02"),
		lastQuestionTime: time.Now().Add(-QuestionInterval), // allow a question immediately
	}
	// Load previously persisted active time for today.
	s.loadDailyActiveTime()

	return s, nil
}

// loadDailyActiveTime loads the total active time (in minutes) from disk.
func (s *Server) loadDailyActiveTime() {
	statsFile := filepath.Join(s.statsDir, s.currentDate+"_time.txt")
	data, err := os.ReadFile(statsFile)
	if err != nil {
		s.totalActive = 0
		_ = os.WriteFile(statsFile, []byte("0"), 0644)
		return
	}
	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		s.totalActive = 0
		return
	}
	// Store as a Duration in minutes.
	s.totalActive = time.Duration(val) * time.Minute
}

// persistDailyActiveTime writes the total active time (in minutes) to disk.
func (s *Server) persistDailyActiveTime() {
	statsFile := filepath.Join(s.statsDir, s.currentDate+"_time.txt")
	minutes := int(s.totalActive.Minutes())
	_ = os.WriteFile(statsFile, []byte(fmt.Sprintf("%d", minutes)), 0644)
}

// updateActiveSession is called on each file event.
func (s *Server) updateActiveSession(now time.Time) {
	s.statsMutex.Lock()
	defer s.statsMutex.Unlock()

	// If the day has changed, flush any active session and reset.
	currentDate := now.Format("2006-01-02")
	if currentDate != s.currentDate {
		s.closeActiveSession(now)
		s.currentDate = currentDate
		s.totalActive = 0
		s.persistDailyActiveTime()
	}

	// If no session is active, start one.
	if s.currentSessionStart.IsZero() {
		s.currentSessionStart = now
		s.currentSessionEnd = now
		return
	}

	// If the gap between this event and the last event is within the idle threshold,
	// update the session's end time.
	if now.Sub(s.currentSessionEnd) <= IdleThreshold {
		s.currentSessionEnd = now
		return
	}

	// Otherwise, the previous session is over.
	s.closeActiveSession(now)
	// Start a new session.
	s.currentSessionStart = now
	s.currentSessionEnd = now
}

// closeActiveSession closes the current session (if any) and adds its duration to the total.
func (s *Server) closeActiveSession(now time.Time) {
	if s.currentSessionStart.IsZero() {
		return
	}
	// Compute the session duration.
	sessionDuration := time.Duration(0)
	if s.currentSessionEnd.Sub(s.currentSessionStart) < IdleThreshold {
		sessionDuration = now.Sub(s.currentSessionStart)
	} else {
		sessionDuration = s.currentSessionEnd.Sub(s.currentSessionStart)
	}
	s.totalActive += sessionDuration
	s.persistDailyActiveTime()
	log.Printf("Closed session: start=%s, end=%s, duration=%s; total active today: %s",
		s.currentSessionStart.Format("15:04:05"),
		s.currentSessionEnd.Format("15:04:05"),
		sessionDuration, s.totalActive)
	// Reset the active session.
	s.currentSessionStart = time.Time{}
	s.currentSessionEnd = time.Time{}
}

// addWatchers recursively adds watchers to directories.
func (s *Server) addWatchers() error {
	for _, dir := range s.directories {
		// Expand ~ if necessary.
		if strings.HasPrefix(dir, "~") {
			home, err := os.UserHomeDir()
			if err == nil {
				dir = filepath.Join(home, dir[1:])
			}
		}
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip error files
			}
			if info.IsDir() {
				base := filepath.Base(path)
				if base == ".git" || base == "node_modules" {
					return filepath.SkipDir
				}
				if err := s.watcher.Add(path); err != nil {
					log.Printf("Error watching %s: %v", path, err)
				}
			}
			return nil
		})
		if err != nil {
			log.Printf("Error walking directory %s: %v", dir, err)
		}
	}
	return nil
}

// startWatcher runs the fsnotify watcher in a goroutine.
func (s *Server) startWatcher() {
	go func() {
		for {
			select {
			case event, ok := <-s.watcher.Events:
				if !ok {
					return
				}
				// For Write and Create events, update the active session.
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					s.updateActiveSession(time.Now())
				}
				// If a new directory is created, add a watcher for it.
				if event.Op&fsnotify.Create != 0 {
					fi, err := os.Stat(event.Name)
					if err == nil && fi.IsDir() {
						_ = s.watcher.Add(event.Name)
					}
				}
			case err, ok := <-s.watcher.Errors:
				if !ok {
					return
				}
				log.Printf("Watcher error: %v", err)
			}
		}
	}()

	// Start a background ticker to check for idle sessions.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for now := range ticker.C {
			s.statsMutex.Lock()
			// If there is an active session and the gap since the last event is greater than IdleThreshold,
			// then close the session.
			if !s.currentSessionStart.IsZero() && now.Sub(s.currentSessionEnd) > IdleThreshold {
				s.statsMutex.Unlock()
				s.closeActiveSession(now)
			} else {
				s.statsMutex.Unlock()
			}
		}
	}()
}

// startPeriodicLogging logs server status periodically.
func (s *Server) startPeriodicLogging() {
	go func() {
		ticker := time.NewTicker(PeriodicLogInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.statsMutex.Lock()
			activeMins := int(s.totalActive.Minutes())
			liveActive := activeMins
			var timeUntilIdle time.Duration
			sessionActive := !s.currentSessionStart.IsZero()
			if sessionActive {
				// Include current session duration
				sessionDuration := time.Since(s.currentSessionStart)
				liveActive += int(sessionDuration.Minutes())
				// Calculate time until idle: IdleThreshold - (now - currentSessionEnd)
				tUntilIdle := IdleThreshold - time.Since(s.currentSessionEnd)
				if tUntilIdle < 0 {
					tUntilIdle = 0
				}
				timeUntilIdle = tUntilIdle
			}
			s.statsMutex.Unlock()
			log.Printf("[Periodic] Date: %s | Total active minutes (closed sessions): %d | Live active minutes: %d | Time until idle: %s",
				s.currentDate, activeMins, liveActive, timeUntilIdle.Truncate(time.Second).String())
		}
	}()
}

// logInteraction writes an interaction to the CSV log.
func (s *Server) logInteraction(timestamp time.Time, folder, question, answer string) error {
	folder = strings.ReplaceAll(folder, ",", ";")
	question = strings.ReplaceAll(question, ",", ";")
	answer = strings.ReplaceAll(answer, ",", ";")
	formatted := timestamp.Format("2006-01-02 15:04:05")

	s.logMutex.Lock()
	defer s.logMutex.Unlock()

	f, err := os.OpenFile(s.logFilePath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write([]string{formatted, folder, question, answer}); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

// ------------------------------
// HTTP API Handlers
// ------------------------------

// GET /updates returns current stats.
func (s *Server) updatesHandler(w http.ResponseWriter, r *http.Request) {
	s.statsMutex.Lock()
	// Compute active minutes from closed sessions.
	activeMins := int(s.totalActive.Minutes())
	liveActive := activeMins
	var timeUntilIdle time.Duration
	// If a session is active, add its duration and compute idle countdown.
	if !s.currentSessionStart.IsZero() {
		sessionDuration := time.Since(s.currentSessionStart)
		liveActive += int(sessionDuration.Minutes())
		tUntilIdle := IdleThreshold - time.Since(s.currentSessionEnd)
		if tUntilIdle < 0 {
			tUntilIdle = 0
		}
		timeUntilIdle = tUntilIdle
	}
	s.statsMutex.Unlock()

	now := time.Now()
	elapsed := now.Sub(s.lastQuestionTime)
	var remaining time.Duration
	if elapsed < QuestionInterval {
		remaining = QuestionInterval - elapsed
	} else {
		remaining = 0
	}

	resp := UpdateResponse{
		ActiveMinutes:     activeMins,
		LiveActiveMinutes: liveActive,
		TimeUntilIdle:     timeUntilIdle.Truncate(time.Second).String(),
		NextQuestionIn:    remaining.Truncate(time.Second).String(),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// GET /question returns a random mindfulness question if allowed.
func (s *Server) questionHandler(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	if now.Sub(s.lastQuestionTime) < QuestionInterval {
		remaining := QuestionInterval - now.Sub(s.lastQuestionTime)
		http.Error(w, fmt.Sprintf("Too soon – next question in %s", remaining.Truncate(time.Second)), http.StatusTooEarly)
		return
	}
	q := questions[rand.Intn(len(questions))]
	s.lastQuestionTime = now

	resp := QuestionResponse{Question: q}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// POST /interaction accepts a JSON payload with the answer.
func (s *Server) interactionHandler(w http.ResponseWriter, r *http.Request) {
	var req InteractionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if len(strings.Fields(req.Answer)) < MinAnswerWords {
		http.Error(w, fmt.Sprintf("Answer too short: please use at least %d words", MinAnswerWords), http.StatusBadRequest)
		return
	}
	timestamp := time.Unix(req.Timestamp, 0)
	if err := s.logInteraction(timestamp, req.Folder, req.Question, req.Answer); err != nil {
		http.Error(w, "Failed to log interaction", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status": "logged"}`)
}

// ------------------------------
// Run the HTTP server
// ------------------------------

func (s *Server) runHTTP(addr string) {
	http.HandleFunc("/updates", s.updatesHandler)
	http.HandleFunc("/question", s.questionHandler)
	http.HandleFunc("/interaction", s.interactionHandler)
	log.Printf("Starting HTTP server on %s...", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// ------------------------------
// Client mode (example)
// ------------------------------

// Color definitions for client output.
var (
	colorReset = "\033[0m"
	colorCyan  = "\033[1;36m"
)

// getClientLogStateFile returns the path of the persistent client log timestamp file.
func getClientLogStateFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".adhd", "last_client_log"), nil
}

// shouldLogClientOutput returns true if at least ClientRateLimitMinutes have passed.
func shouldLogClientOutput() bool {
	stateFile, err := getClientLogStateFile()
	if err != nil {
		return true // fallback: log output
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return true
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return true
	}
	lastLog := time.Unix(ts, 0)
	return time.Since(lastLog) >= ClientRateLimitMinutes*time.Minute
}

// updateClientLogTimestamp updates the persistent client log timestamp.
func updateClientLogTimestamp() {
	stateFile, err := getClientLogStateFile()
	if err != nil {
		return
	}
	_ = os.WriteFile(stateFile, []byte(fmt.Sprintf("%d", time.Now().Unix())), 0644)
}

// clientGetUpdates shows update information (only prints if rate limit allows).
func clientGetUpdates(serverAddr string) error {
	if !shouldLogClientOutput() {
		return nil
	}
	resp, err := http.Get("http://" + serverAddr + "/updates")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var up UpdateResponse
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil {
		return err
	}
	// Print with an extra newline and color.
	output := fmt.Sprintf("\n%s🛠️  Active minutes today: %d, Live active minutes: %d, Time until idle: %s, next question in: %s%s\n",
		colorCyan, up.ActiveMinutes, up.LiveActiveMinutes, up.TimeUntilIdle, up.NextQuestionIn, colorReset)
	fmt.Print(output)
	updateClientLogTimestamp()
	return nil
}

// clientGetQuestion fetches a question from the server.
func clientGetQuestion(serverAddr string) (string, error) {
	resp, err := http.Get("http://" + serverAddr + "/question")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s", body)
	}
	var qResp QuestionResponse
	if err := json.NewDecoder(resp.Body).Decode(&qResp); err != nil {
		return "", err
	}
	// Print the question in color with a preceding empty line.
	output := fmt.Sprintf("\n%s=== ADHD Checkpoint ===\n%s%s\n",
		colorCyan, qResp.Question, colorReset)
	fmt.Print(output)
	return qResp.Question, nil
}

// clientPostInteraction sends an interaction response.
func clientPostInteraction(serverAddr string, folder, question, answer string) error {
	reqBody := InteractionRequest{
		Timestamp: time.Now().Unix(),
		Folder:    folder,
		Question:  question,
		Answer:    answer,
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	resp, err := http.Post("http://"+serverAddr+"/interaction", "application/json", strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	output := fmt.Sprintf("\n%sServer response: %s%s\n", colorCyan, string(body), colorReset)
	fmt.Print(output)
	return nil
}

// ------------------------------
// Main function
// ------------------------------

func main() {
	mode := flag.String("mode", "server", "Mode: server or client")
	addr := flag.String("addr", "localhost:8080", "HTTP server address")
	force := flag.Bool("force", false, "Force question prompt if available")
	flag.Parse()

	switch *mode {
	case "server":
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("Cannot get home directory: %v", err)
		}
		watchDirs := []string{
			filepath.Join(home, "Documents"),
			filepath.Join(home, "dotfiles"),
		}
		server, err := NewServer(watchDirs)
		if err != nil {
			log.Fatalf("Error initializing server: %v", err)
		}
		if err := server.addWatchers(); err != nil {
			log.Fatalf("Error adding watchers: %v", err)
		}
		server.startWatcher()
		// Start periodic server logging.
		server.startPeriodicLogging()

		// On receiving an exit signal, close any active session and exit.
		exitChan := make(chan os.Signal, 1)
		signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-exitChan
			log.Println("Shutting down: closing active session (if any) and persisting data.")
			server.statsMutex.Lock()
			server.closeActiveSession(time.Now())
			server.statsMutex.Unlock()
			os.Exit(0)
		}()

		server.runHTTP(*addr)
	case "client":
		if *force {
			question, err := clientGetQuestion(*addr)
			if err != nil {
				_ = clientGetUpdates(*addr)
				return
			}
			reader := bufio.NewReader(os.Stdin)
			var answer string
			for {
				fmt.Print("Please enter your answer (at least 5 words): ")
				answer, err = reader.ReadString('\n')
				if err != nil {
					log.Fatalf("Error reading answer: %v", err)
				}
				answer = strings.TrimSpace(answer)
				if len(strings.Fields(answer)) < MinAnswerWords {
					fmt.Printf("Your answer is too short. Please provide at least %d words.\n", MinAnswerWords)
					continue
				}
				break
			}
			cwd, _ := os.Getwd()
			if err := clientPostInteraction(*addr, cwd, question, answer); err != nil {
				log.Fatalf("Error posting interaction: %v", err)
			}
		} else {
			if err := clientGetUpdates(*addr); err != nil {
				log.Fatalf("Error fetching updates: %v", err)
			}
		}
	default:
		log.Fatalf("Unknown mode: %s", *mode)
	}
}
