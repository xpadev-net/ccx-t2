package ledger

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// rawTask holds unparsed front matter YAML text and the body text.
type rawTask struct {
	frontMatter string
	body        string
}

// parse splits ledger content into raw task pairs.
// Rules:
//   - A "---" line followed by a line starting with "id:" begins a front matter block.
//   - A "---" line inside a front matter block ends it (regardless of next line).
//   - All other "---" lines are treated as horizontal rules in the body.
//   - EOF terminates the last body.
func parse(content string) ([]rawTask, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	var tasks []rawTask

	const (
		stateBody      = 0
		stateFrontMatter = 1
	)

	state := stateBody
	var currentFM strings.Builder
	var currentBody strings.Builder
	inTask := false // whether we have an active task being built

	isSep := func(line string) bool {
		return line == "---"
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		switch state {
		case stateBody:
			if isSep(line) {
				// Peek at next line: if it starts with "id:", this begins a front matter block.
				if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "id:") {
					// Save the current task if we have one.
					if inTask {
						tasks = append(tasks, rawTask{
							frontMatter: currentFM.String(),
							body:        currentBody.String(),
						})
						currentFM.Reset()
						currentBody.Reset()
					}
					// Transition to front matter state (don't include this "---" in FM).
					state = stateFrontMatter
					inTask = true
				} else {
					// Treat as horizontal rule in body.
					if inTask {
						currentBody.WriteString(line)
						currentBody.WriteString("\n")
					}
				}
			} else {
				if inTask {
					currentBody.WriteString(line)
					currentBody.WriteString("\n")
				}
			}

		case stateFrontMatter:
			if isSep(line) {
				// End of front matter.
				state = stateBody
			} else {
				currentFM.WriteString(line)
				currentFM.WriteString("\n")
			}
		}
	}

	// Flush remaining task.
	if inTask {
		if state == stateFrontMatter {
			// Front matter was opened but never closed — treat the accumulated
			// content as a partial front matter and return an error to the caller.
			return nil, fmt.Errorf("unclosed front matter block (missing closing '---')")
		}
		tasks = append(tasks, rawTask{
			frontMatter: currentFM.String(),
			body:        currentBody.String(),
		})
	}

	return tasks, nil
}

// unmarshalTask parses YAML front matter into a Task.
func unmarshalTask(fm string) (Task, error) {
	var t Task
	if err := yaml.Unmarshal([]byte(fm), &t); err != nil {
		return Task{}, err
	}
	return t, nil
}

// marshalFrontMatter serializes a Task's front matter with "id" always first.
func marshalFrontMatter(t Task) ([]byte, error) {
	// Build ordered map manually to guarantee id is first.
	// Encode the ID as a YAML scalar to handle any special characters safely.
	var buf bytes.Buffer
	idNode := &yaml.Node{Kind: yaml.ScalarNode, Value: t.ID, Tag: "!!str"}
	idBytes, err := yaml.Marshal(idNode)
	if err != nil {
		return nil, fmt.Errorf("marshal id: %w", err)
	}
	buf.WriteString("id: ")
	// yaml.Marshal of a scalar node produces "value\n"; strip the trailing newline.
	buf.Write(bytes.TrimRight(idBytes, "\n"))
	buf.WriteString("\n")

	// Marshal the rest of the struct, then strip the id line.
	rest, err := yaml.Marshal(t)
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(bytes.NewReader(rest))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "id:" || strings.HasPrefix(line, "id: ") {
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("marshal front matter: scanner error: %w", err)
	}

	return buf.Bytes(), nil
}
