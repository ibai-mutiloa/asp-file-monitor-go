package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

var (
	watchDir   string
	repoDir    string
	interval   int
	maxWait    int
	extensions string
	verbose    bool
)

func init() {
	flag.StringVar(&watchDir, "dir", ".", "Directorio a vigilar (por defecto .)")
	flag.StringVar(&repoDir, "repo", ".", "Directorio del repo git (por defecto .)")
	flag.IntVar(&interval, "interval", 180, "Intervalo de debounce en segundos antes de hacer commit (por defecto 120s)")
	flag.IntVar(&maxWait, "max-wait", 900, "Tiempo máximo en segundos desde el primer cambio para forzar un commit (por defecto 600s)")
	flag.StringVar(&extensions, "ext", ".asp", "Extensiones a vigilar separadas por coma (por defecto .asp)")
	flag.BoolVar(&verbose, "verbose", false, "Modo verbose para más logs")
}

func main() {
	flag.Parse()

	// Validar que estamos en un repo git
	if !isGitRepo(repoDir) {
		log.Fatalf("El directorio %s no es un repositorio git válido", repoDir)
	}

	// Parsear extensiones
	exts := parseExtensions(extensions)

	log.Printf("Agente Git iniciado")
	log.Printf("Vigilando: %s", watchDir)
	log.Printf("Repositorio: %s", repoDir)
	log.Printf("Debounce: %ds | Max-wait: %ds", interval, maxWait)
	log.Printf("Extensiones: %v", exts)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()

	// Estado del agente
	state := &agentState{
		changed:    make(map[string]struct{}),
		extensions: exts,
	}

	// Timers
	commitTimer := time.NewTimer(time.Hour)
	commitTimer.Stop()
	var maxTimer *time.Timer
	var maxTimerCh <-chan time.Time
	var firstChangeTime time.Time

	scheduleCommit := func() {
		if !commitTimer.Stop() {
			select {
			case <-commitTimer.C:
			default:
			}
		}
		commitTimer.Reset(time.Duration(interval) * time.Second)

		// Iniciar max-wait timer en el primer cambio
		state.mu.Lock()
		isFirstChange := len(state.changed) == 1
		state.mu.Unlock()

		if maxWait > 0 && isFirstChange {
			firstChangeTime = time.Now()
			if maxTimer != nil {
				maxTimer.Stop()
			}
			maxTimer = time.NewTimer(time.Duration(maxWait) * time.Second)
			maxTimerCh = maxTimer.C
			if verbose {
				log.Printf("Max-wait timer iniciado: commit forzado en %ds", maxWait)
			}
		}
	}

	// Agregar watchers recursivamente
	if err := addRecursive(watcher, watchDir); err != nil {
		log.Fatalf("Error al añadir watchers: %v", err)
	}

	done := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				handleEvent(event, watcher, state, scheduleCommit)

			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("Watcher error:", err)

			case <-commitTimer.C:
				performCommit(state, &maxTimer, &maxTimerCh, "debounce completado")

			case <-maxTimerCh:
				if maxTimer != nil {
					maxTimer = nil
					maxTimerCh = nil
				}
				elapsed := time.Since(firstChangeTime)
				performCommit(state, &maxTimer, &maxTimerCh,
					fmt.Sprintf("max-wait alcanzado (%.0fs)", elapsed.Seconds()))

			case s := <-sig:
				log.Printf("Señal %v recibida, terminando...", s)
				commitTimer.Stop()
				if maxTimer != nil {
					maxTimer.Stop()
				}
				performCommit(state, &maxTimer, &maxTimerCh, "flush on exit")
				close(done)
				return
			}
		}
	}()

	<-done
	log.Println("Agente detenido correctamente")
}

type agentState struct {
	mu         sync.Mutex
	changed    map[string]struct{}
	extensions map[string]bool
}

func handleEvent(event fsnotify.Event, watcher *fsnotify.Watcher, state *agentState, scheduleCommit func()) {
	// Directorios nuevos
	if event.Op&fsnotify.Create == fsnotify.Create {
		info, err := os.Stat(event.Name)
		if err == nil && info.IsDir() {
			_ = addRecursive(watcher, event.Name)
			if verbose {
				log.Printf("Nuevo directorio añadido al watcher: %s", event.Name)
			}
			return
		}
	}

	// Cambios en archivos relevantes
	if matchesExtension(event.Name, state.extensions) &&
		(event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename)) != 0 {
		state.mu.Lock()
		state.changed[event.Name] = struct{}{}
		count := len(state.changed)
		state.mu.Unlock()

		log.Printf("Cambio detectado [%d]: %s", count, filepath.Base(event.Name))
		scheduleCommit()
	}
}

func performCommit(state *agentState, maxTimer **time.Timer, maxTimerCh *<-chan time.Time, reason string) {
	state.mu.Lock()
	files := make([]string, 0, len(state.changed))
	for f := range state.changed {
		files = append(files, f)
	}
	state.changed = make(map[string]struct{})
	state.mu.Unlock()

	// Limpiar max timer
	if *maxTimer != nil {
		(*maxTimer).Stop()
		*maxTimer = nil
		*maxTimerCh = nil
	}

	if len(files) == 0 {
		if verbose {
			log.Printf("%s: sin cambios pendientes", reason)
		}
		return
	}

	log.Printf("Commit iniciado (%s): %d archivo(s)", reason, len(files))
	start := time.Now()

	if err := gitAddCommitPush(repoDir, files); err != nil {
		log.Printf("Git error: %v", err)
	} else {
		log.Printf("Commit y push completado en %.2fs", time.Since(start).Seconds())
	}
}

func matchesExtension(name string, extensions map[string]bool) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return extensions[ext]
}

func parseExtensions(input string) map[string]bool {
	exts := make(map[string]bool)
	for _, ext := range strings.Split(input, ",") {
		ext = strings.TrimSpace(ext)
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		exts[strings.ToLower(ext)] = true
	}
	return exts
}

func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := filepath.Base(path)
			// Ignorar directorios comunes que no necesitan vigilancia
			if base == ".git" || base == "node_modules" || base == "logs" ||
				base == "tmp" || base == "temp" {
				return filepath.SkipDir
			}
			if err := w.Add(path); err != nil && verbose {
				log.Printf("No se pudo añadir watcher a %s: %v", path, err)
			}
		}
		return nil
	})
}

func isGitRepo(repo string) bool {
	gitDir := filepath.Join(repo, ".git")
	info, err := os.Stat(gitDir)
	return err == nil && info.IsDir()
}

func gitAddCommitPush(repo string, files []string) error {
	// Git add
	args := append([]string{"add", "--"}, files...)
	if out, err := runGit(repo, args...); err != nil {
		return fmt.Errorf("git add falló: %v -> %s", err, out)
	}

	// Git status para verificar cambios
	if out, err := runGit(repo, "status", "--short"); err == nil && strings.TrimSpace(out) == "" {
		if verbose {
			log.Println("Sin cambios staged para commit")
		}
		return nil
	}

	// Commit con mensaje descriptivo
	fileList := ""
	if len(files) <= 3 {
		fileNames := make([]string, len(files))
		for i, f := range files {
			fileNames[i] = filepath.Base(f)
		}
		fileList = strings.Join(fileNames, ", ")
	} else {
		fileList = fmt.Sprintf("%d archivos", len(files))
	}

	msg := fmt.Sprintf("Auto-commit: %s [%s]", fileList, time.Now().Format("2006-01-02 15:04:05"))
	if out, err := runGit(repo, "commit", "-m", msg); err != nil {
		if !isNoChanges(out) {
			return fmt.Errorf("git commit falló: %v -> %s", err, out)
		}
		return nil
	}

	// Push
	if out, err := runGit(repo, "push"); err != nil {
		return fmt.Errorf("git push falló: %v -> %s", err, out)
	}

	return nil
}

func runGit(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if verbose {
		log.Printf("Ejecutando: git %s", strings.Join(args, " "))
	}

	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

func isNoChanges(output string) bool {
	l := strings.ToLower(output)
	return strings.Contains(l, "nothing to commit") ||
		strings.Contains(l, "no changes added to commit")
}
