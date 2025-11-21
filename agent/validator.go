// ================================================
// VALIDADOR ASP – Análisis estático
// ================================================
package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var includeRegex = regexp.MustCompile(`<!--#include (file|virtual)="([^"]+)"-->`)
var ifRegex = regexp.MustCompile(`(?i)\bif\b`)
var endIfRegex = regexp.MustCompile(`(?i)\bend if\b`)
var forRegex = regexp.MustCompile(`(?i)\bfor\b`)
var nextRegex = regexp.MustCompile(`(?i)\bnext\b`)

func ValidateASPWithCScript(files []string) []string {
	var errors []string

	for _, file := range files {
		if strings.ToLower(filepath.Ext(file)) != ".asp" {
			continue
		}

		cmd := exec.Command("cscript.exe", "//nologo", file)
		output, err := cmd.CombinedOutput()

		if err != nil {
			errors = append(errors, fmt.Sprintf("[CSCRIPT] %s → %v\n%s", file, err, string(output)))
		}
	}

	return errors
}

func ValidateASPFiles(files []string) []string {
	var errors []string

	for _, file := range files {
		if strings.ToLower(filepath.Ext(file)) != ".asp" {
			continue
		}

		content, err := ioutil.ReadFile(file)
		if err != nil {
			errors = append(errors, fmt.Sprintf("[ERROR] No se pudo leer %s: %v", file, err))
			continue
		}

		text := string(content)

		// 1️⃣ Validar includes rotos
		matches := includeRegex.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			path := m[2]

			// Resolver ruta relativa
			resolved := filepath.Join(filepath.Dir(file), path)
			if _, err := os.Stat(resolved); err != nil {
				errors = append(errors,
					fmt.Sprintf("[INCLUDE] En %s → archivo no encontrado: %s", file, path))
			}
		}

		// 2️⃣ Validar If / End If balanceados
		ifCount := len(ifRegex.FindAllString(text, -1))
		endifCount := len(endIfRegex.FindAllString(text, -1))
		if ifCount != endifCount {
			errors = append(errors,
				fmt.Sprintf("[SINTAXIS] En %s → IF (%d) y END IF (%d) no coinciden", file, ifCount, endifCount))
		}

		// 3️⃣ Validar For / Next balanceados
		forCount := len(forRegex.FindAllString(text, -1))
		nextCount := len(nextRegex.FindAllString(text, -1))
		if forCount != nextCount {
			errors = append(errors,
				fmt.Sprintf("[SINTAXIS] En %s → FOR (%d) y NEXT (%d) no coinciden", file, forCount, nextCount))
		}

		// 4️⃣ Detectar comillas curvas no válidas
		if strings.Contains(text, "“") || strings.Contains(text, "”") {
			errors = append(errors, fmt.Sprintf("[UNICODE] En %s → comillas inválidas detectadas", file))
		}
	}

	return errors
}
