// adhd.go
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
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ------------------------------
// Types and constants
// ------------------------------

const (
	QuestionInterval = 10 * time.Minute
	MinAnswerWords   = 5
	ActiveMaxDiff    = 5 // minutes: maximum added even if a long gap
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

// API response types

type UpdateResponse struct {
	RecentFileChanges int    `json:"recent_file_changes"`
	ActiveMinutes     int    `json:"active_minutes"`
	NextQuestionIn    string `json:"next_question_in"` // as human-readable
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
	logFilePath       string
	statsDir          string
	logMutex          sync.Mutex
	statsMutex        sync.Mutex
	todayActiveMins   int
	lastIncrementTime time.Time
	lastQuestionTime  time.Time
	// For file-change notifications we count changes in a short window:
	recentChanges     int
	recentChangesLock sync.Mutex

	// watcher and watched directories
	watcher     *fsnotify.Watcher
	directories []string

	// used to reset daily stats
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
		// Write CSV header.
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

	server := &Server{
		logFilePath:       logFilePath,
		statsDir:          adhdDir,
		watcher:           watcher,
		directories:       watchDirs,
		currentDate:       time.Now().Format("2006-01-02"),
		lastIncrementTime: time.Now(),
		lastQuestionTime:  time.Now().Add(-QuestionInterval), // so that a question is allowed immediately
	}

	return server, nil
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
				// Skip some directories (like .git, node_modules)
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

// Start the file watcher in a goroutine.
func (s *Server) startWatcher() {
	go func() {
		for {
			select {
			case event, ok := <-s.watcher.Events:
				if !ok {
					return
				}
				// We consider Create and Write events.
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					s.handleFileEvent(event)
				}
				// If a new directory is created, add watcher for it.
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
}

// handleFileEvent processes a file change event.
func (s *Server) handleFileEvent(event fsnotify.Event) {
	slog.Info("new file event", "event", event.Name)
	s.recentChangesLock.Lock()
	s.recentChanges++
	s.recentChangesLock.Unlock()

	now := time.Now()

	s.statsMutex.Lock()
	// Reset daily stats if day changed.
	today := now.Format("2006-01-02")
	if today != s.currentDate {
		s.currentDate = today
		s.todayActiveMins = 0
		s.lastIncrementTime = now
	}
	// Compute minutes passed since last increment.
	diff := now.Sub(s.lastIncrementTime)
	if diff >= time.Minute {
		mins := int(diff.Minutes())
		if mins > ActiveMaxDiff {
			mins = ActiveMaxDiff
		}
		s.todayActiveMins += mins
		s.lastIncrementTime = now
		log.Printf("File change detected (%s). Added %d minute(s). Total active minutes today: %d", event.Name, mins, s.todayActiveMins)
		// Optionally, you could write the updated value to a stats file.
	}
	s.statsMutex.Unlock()
}

// logInteraction writes an interaction to the CSV log.
func (s *Server) logInteraction(timestamp time.Time, folder, question, answer string) error {
	// Replace commas with semicolons.
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
	err = w.Write([]string{formatted, folder, question, answer})
	if err != nil {
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
	activeMins := s.todayActiveMins
	s.statsMutex.Unlock()

	s.recentChangesLock.Lock()
	recent := s.recentChanges
	// Reset recent changes counter after reporting.
	s.recentChanges = 0
	s.recentChangesLock.Unlock()

	// Compute time remaining until next question.
	now := time.Now()
	elapsed := now.Sub(s.lastQuestionTime)
	var remaining time.Duration
	if elapsed < QuestionInterval {
		remaining = QuestionInterval - elapsed
	} else {
		remaining = 0
	}

	resp := UpdateResponse{
		RecentFileChanges: recent,
		ActiveMinutes:     activeMins,
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
	// Pick a random question.
	q := questions[rand.Intn(len(questions))]
	s.lastQuestionTime = now

	resp := QuestionResponse{Question: q}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// POST /interaction accepts a JSON payload with the answer.
func (s *Server) interactionHandler(w http.ResponseWriter, r *http.Request) {
	var req InteractionRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Validate that answer has at least MinAnswerWords.
	words := strings.Fields(req.Answer)
	if len(words) < MinAnswerWords {
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

// clientGetQuestion shows how a client might use the API.
func clientGetQuestion(serverAddr string) error {
	resp, err := http.Get("http://" + serverAddr + "/question")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("error: %s", body)
	}
	var qResp QuestionResponse
	err = json.NewDecoder(resp.Body).Decode(&qResp)
	if err != nil {
		return err
	}
	fmt.Printf("\n=== ADHD Checkpoint ===\n%s\n", qResp.Question)
	return nil
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
	fmt.Printf("Server response: %s\n", body)
	return nil
}

// clientGetUpdates shows how to get update information.
func clientGetUpdates(serverAddr string) error {
	resp, err := http.Get("http://" + serverAddr + "/updates")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var up UpdateResponse
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil {
		return err
	}
	fmt.Printf("Recent file changes: %d\nActive minutes today: %d\nNext question in: %s\n",
		up.RecentFileChanges, up.ActiveMinutes, up.NextQuestionIn)
	return nil
}

// ------------------------------
// Main function
// ------------------------------

func main() {
	// Use a command-line flag "mode" to select server or client.
	mode := flag.String("mode", "server", "Mode: server or client")
	addr := flag.String("addr", "localhost:8080", "HTTP server address")
	flag.Parse()

	switch *mode {
	case "server":
		// Example directories to watch.
		// (You can adjust these as needed.)
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("Cannot get home directory: %v", err)
		}
		// In the original bash script, project_paths were something like:
		//   "${HOME}Documents/*" and "${HOME}dotfiles"
		// Here we watch the Documents directory and dotfiles directory.
		watchDirs := []string{
			filepath.Join(home, "Documents"),
			filepath.Join(home, "dotfiles"),
		}
		server, err := NewServer(watchDirs)
		if err != nil {
			log.Fatalf("Error initializing server: %v", err)
		}
		err = server.addWatchers()
		if err != nil {
			log.Fatalf("Error adding watchers: %v", err)
		}
		server.startWatcher()
		server.runHTTP(*addr)
	case "client":
		// A very simple command-line client.
		// For example, the client can get updates, get a question, and post an interaction.
		fmt.Println("Fetching update status from server...")
		if err := clientGetUpdates(*addr); err != nil {
			log.Fatalf("Error fetching updates: %v", err)
		}
		fmt.Println("Fetching a mindfulness question from server...")
		if err := clientGetQuestion(*addr); err != nil {
			log.Fatalf("Error getting question: %v", err)
		}
		// In a real client, you might now prompt the user to enter an answer.
		// Here we simulate by reading from stdin using bufio.
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Please enter your answer (at least 5 words): ")
		answer, err := reader.ReadString('\n')
		if err != nil {
			log.Fatalf("Error reading answer: %v", err)
		}
		answer = strings.TrimSpace(answer)
		// For demonstration, we use the current working directory and the question we just got.
		cwd, _ := os.Getwd()
		// In a real implementation, you should capture the actual question that was returned.
		// Here we use a placeholder.
		question := "Placeholder Question (see server log)"
		if err := clientPostInteraction(*addr, cwd, question, answer); err != nil {
			log.Fatalf("Error posting interaction: %v", err)
		}
	default:
		log.Fatalf("Unknown mode: %s", *mode)
	}
}
