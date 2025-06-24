package main

/*
This script:
- reads (deck, url) pairs from the file `urlFile`.
- downlaods for each pair the corresponding HTML source (in parallel)
- creates cards for each constant block, variable block, function block and type block found in a pairs HTML source (in parallel).
- for each pair adds all found cards to the given deck via AnkiConnect. 
*/

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/atselvan/ankiconnect"
	"github.com/ericchiang/css"
	"golang.org/x/net/html"

	HTMLTrees "gostdlibintoankicards/pkg"
)

// starts `workerCount` on channel `in` competing go routines that publish to `out`. 
func Parallel[T any](out chan<-T, in <-chan T, parallel func(chan<-T, <-chan T), workerCount int) {
	for i := 0; i < workerCount; i++ {
		go parallel(out, in)
	}
}

const (
	urlFile = "./urls_1.22.0.txt" // TODO: change this to your URL's file.
)

// datatype passed between pipeline components
type Task struct {
	url, deck string 
	html []byte
	notes []ankiconnect.Note
	err error
}

func (t *Task) ImportPath() string {
	res := strings.SplitN(t.deck, "::", 3)
	if len(res) < 3 {
		log.Fatalf("Task::ImportPath:: expected at least 3 DeckParts, got '%s'\n", t.deck)
	}
	return strings.ToLower(strings.ReplaceAll(res[2], "::", "."))
}

func (t *Task) AddNote(front, back, impl string) {
	t.notes = append(t.notes, ankiconnect.Note{
		DeckName: t.deck,
		ModelName: "Golang", 
		Fields: ankiconnect.Fields{
			"Identifier": front,
			"Declaration": back,
			"Implementation": impl,
		},
	})
	//fmt.Printf("--------------------\n%s\n---------------\n%s\n\n", front, back)
}

func (t Task) String() string {
	return fmt.Sprintf("Task{ deck: %s, err: %v }", t.deck, t.err)
}

func NewTask(url, deck string) Task {
	return Task{
		url: url,
		deck: deck,
		notes: make([]ankiconnect.Note, 0),
		err: nil,
	}
}

// construct and run pipeline
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	client := ankiconnect.NewClient()
	err := client.Ping()
	if err != nil {
		log.Fatal("main::client.Ping::", err)
	}
	log.Println("Connected Anki Client")

	downloadQueue := make(chan Task, 100)
	processQueue := make(chan Task, 100)
	ankiQueue := make(chan Task, 1000)

	go TaskGenerator(urlFile, downloadQueue)
	go Parallel(processQueue, downloadQueue, HtmlDownloader, 5)	
	go Parallel(ankiQueue, processQueue, HtmlProcessor, 10)

	go NoteUploader(client, ankiQueue)

	fmt.Println("[ press enter to exit ]")
	fmt.Scanln()
}

// reads urls from file and wraps them in a task instance.
func TaskGenerator(fp string, out chan<-Task) {
	file, err := os.Open(fp)
	if err != nil {
		log.Fatal("TaskGenerator::", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	task_count := 0
	for scanner.Scan() {
		line := scanner.Text()
		var url, deck string
		n, err := fmt.Sscanf(line, "%s %s", &deck, &url)
		if n == 0 && err == io.EOF { // ignore empty lines
			continue
		}
		if err != nil {
			log.Fatal("TaskGenerator::", err)
		}
		task := NewTask(url, deck)
		out <- task
		task_count++
	}
	log.Printf("'%s' loaded file, %d tasks created\n", fp, task_count)
}

// download HTML source, found at the tasks url, for any given task instance
func HtmlDownloader(out chan<-Task, in <-chan Task) {
	task := <-in
	Outer: for {
		resp, err := http.Get(task.url)
		if err != nil {
			task.err = fmt.Errorf("HtmlDownloader::failed to downlaod html for task %v: %w", task, err)
			out <- task
			continue
		}
		
		// handle response code
		switch resp.StatusCode {
			case 200:
			case 429:
				time.Sleep(500 * time.Millisecond)
				continue Outer
			default: 
			log.Fatal(resp.Status)
		}

		html, err := io.ReadAll(resp.Body)
		if err != nil {
			task.err = fmt.Errorf("HtmlDownloader::failed to read html body for %v: %w", task, err)
			out <- task
			continue
		}
		task.html = html
		log.Printf("'%s' downloaded documentation (%v bytes)\n", task.url, len(task.html))
		out<-task
		resp.Body.Close()
		task = <-in
	}
}

// parse a tasks HTML source and add a Anki note to the task for each constant block, variable block, function block and type block found  
func HtmlProcessor(out chan<- Task, in <-chan Task) {
	for task := range in {
		root, err := html.Parse(bytes.NewBuffer(task.html))
		if err != nil {
			log.Fatal("HTMLProcessor::root::", err)
		}
		
		// local hrefs to global hrefs
		
		base, err := url.Parse(task.url)
		HTMLTrees.Modify(root, func(node *html.Node) (res error) {
			res = nil
			for i := 0; i < len(node.Attr); i++ {
				if node.Attr[i].Key == "href" {
					link, err := url.Parse(node.Attr[i].Val)
					if err == nil {
						target := base.ResolveReference(link)
						node.Attr[i].Val = target.String()
						//fmt.Printf("Debug: %#v\n", target.String())
					}
				}
				i++
			}
			return
		})

		// selectors 

		doc_src_header, err := css.Parse("a.Documentation-source")
		if err != nil {
			log.Fatal("HTMLProcessor::doc_src_header::", err)
		}
		doc_src_add_prefix := func(root *html.Node, name string) {
			nodes := doc_src_header.Select(root)
			if len(nodes) == 0 {
				log.Fatalf("HTMLProcessor::doc_src_add_prefix::no nodes found\n")
			}
			for _, node := range nodes {
				//fmt.Printf("Debug: %s\n", HTMLTrees.HTMLString(node))
				node.FirstChild.Data = name + "." + node.FirstChild.Data
				//fmt.Printf("Debug: %s\n", HTMLTrees.HTMLString(node))
			}
		}

		// variables 

		var_selector, err := css.Parse("section.Documentation-variables div.Documentation-declaration")
		if err != nil {
			log.Fatal("HTMLProcessor::var_selector::", err)
		}
		var_span_selector, err := css.Parse("span[data-kind='variable']")
		if err != nil {
			log.Fatal(err)
		}
		
		variables := var_selector.Select(root)
		//fmt.Printf("found %d variables\n", len(variables))

		for i := 0; i < len(variables); i++ {
			variable := variables[i]

			// append deck importPath as prefix to variable name
			for _, span := range var_span_selector.Select(variable) {
				id, err := GetHtmlAttributeByKey(span, "id")
				if err != nil {
					log.Fatal(err)
				}
				pattern := regexp.MustCompile(fmt.Sprintf(`(?P<id>%s)`,id.Val))
				nodes := HTMLTrees.MatchingNodes(span, pattern)
				//fmt.Println("debug: len(nodes) = ", len(nodes))
				for _, node := range nodes {
					node.Data = pattern.ReplaceAllString(node.Data, task.ImportPath() + ".${id}")
					//fmt.Println("debug: ", node.Data)
				}
			}

			// find following <p>...</p>
			nodes := []*html.Node{variable}
			for c := variable.NextSibling.NextSibling; c != nil && c.Data == "p"; c = c.NextSibling.NextSibling { // skip whitspace div
				nodes = append(nodes, c)
			}

			front := HTMLTrees.HTMLString(
				HTMLTrees.DeepCopySubtrees(root, nodes),
			)

			task.AddNote(front, front, "")
		}

		// constants

		const_selector, err := css.Parse("section.Documentation-constants div.Documentation-declaration")
		if err != nil {
			log.Fatal("HTMLProcessor::const_selector::", err)
		}
		const_span_selector, err := css.Parse("span[data-kind='constant']")
		if err != nil {
			log.Fatal(err)
		}

		constants := const_selector.Select(root)
		//fmt.Printf("found %d constants\n", len(constants))

		for i := 0; i < len(constants); i++ {
			constant := constants[i]

			// append deck importPath as prefix to variable name
			for _, span := range const_span_selector.Select(constant) {
				id, err := GetHtmlAttributeByKey(span, "id")
				if err != nil {
					log.Fatal(err)
				}
				pattern := regexp.MustCompile(fmt.Sprintf(`(?P<id>%s)`,id.Val))
				nodes := HTMLTrees.MatchingNodes(span, pattern)
				//fmt.Println("debug: len(nodes) = ", len(nodes))
				for _, node := range nodes {
					node.Data = pattern.ReplaceAllString(node.Data, task.ImportPath() + ".${id}")
					//fmt.Println("debug: ", node.Data)
				}
			}

			// find following <p>...</p>
			nodes := []*html.Node{constant}
			for c := constant.NextSibling.NextSibling; c != nil && c.Data == "p"; c = c.NextSibling.NextSibling { // skip whitspace div
				nodes = append(nodes, c)
			}

			front := HTMLTrees.HTMLString(
				HTMLTrees.DeepCopySubtrees(root, nodes),
			)

			task.AddNote(front, front, "")
		}


		// functions

		func_selector, err := css.Parse("div.Documentation-function")
		if err != nil {
			log.Fatal("HTMLProcessor::func_selector::", err)
		}
		functions := func_selector.Select(root)
		//fmt.Printf("found %d functions\n", len(functions))

		func_header_selector, err := css.Parse("div.Documentation-function h4.Documentation-functionHeader")
		if err != nil {
			log.Fatal(err)
		}
		func_headers := func_header_selector.Select(root)
		if len(func_headers) != len(functions) {
			log.Fatalf("HTMLProcessor::unexpected_amount_of_func_headers:: found %d functions and %d headers\n", len(functions), len(func_headers))
		}
		for i := 0; i < len(functions); i++ {
			function := functions[i]
			header := func_headers[i]
			doc_src_add_prefix(header, task.ImportPath())

			back := HTMLTrees.HTMLString(
				HTMLTrees.DeepCopySubtrees(root, []*html.Node{function}),
			)
			front := HTMLTrees.HTMLString(
				HTMLTrees.DeepCopySubtrees(root, []*html.Node{header}),
			)
			task.AddNote(front, back, "")
		}

		// types

		type_selector, err := css.Parse("div.Documentation-type")
		if err != nil {
			log.Fatal(err)
		}
		types := type_selector.Select(root)
		//fmt.Printf("found %d types\n", len(functions))

		type_header_selector, err := css.Parse("div.Documentation-type h4.Documentation-typeHeader")
		if err != nil {
			log.Fatal("HTMLProcessor::type_header_selector::", err)
		}
		type_headers := type_header_selector.Select(root)
		if len(type_headers) != len(types) {
			log.Fatalf("HTMLProcessor::unexpected_amount_of_type_headers:: %d types and %d headers\n", len(types), len(type_headers))
		}
		for i := 0; i < len(types); i++ {
			type_ := types[i]
			header := type_headers[i]
			doc_src_add_prefix(header, task.ImportPath())
			back := HTMLTrees.HTMLString(
				HTMLTrees.DeepCopySubtrees(root, []*html.Node{type_}),
			)
			front := HTMLTrees.HTMLString(
				HTMLTrees.DeepCopySubtrees(root, []*html.Node{header}),
			)
			task.AddNote(front, back, "")
		}

		log.Printf(
			"'%s' found %d variables, %d constants, %d functions, %d types. Generated %d notes", 
			task.deck, len(variables), len(constants), len(functions), len(types), len(task.notes),
		)
		out <- task 
	}

}

func GetHtmlAttribute(node *html.Node, f func(attr html.Attribute) bool) (*html.Attribute, error) {
	for _, attr := range node.Attr {
		if f(attr) {
			return &attr, nil
		}
	}
	return nil, errors.New("no matching attribute found")
}

func GetHtmlAttributeByKey(node *html.Node, key string) (*html.Attribute, error) {
	return GetHtmlAttribute(node, func(attr html.Attribute) bool {
		return attr.Key == key
	})
}

// for each task ensure the associated Anki deck exists and upload all Anki notes from `task` to the specified deck.
func NoteUploader(client *ankiconnect.Client, in <-chan Task) {
	decks, err := client.Decks.GetAll()
	if err != nil {
		log.Fatal("NoteUploader::DeckRequestFailed::", err)
	}
	for task := range in {
		if !slices.Contains(*decks, task.deck) {
			err := client.Decks.Create(task.deck)
			if err != nil {
				log.Fatal("NoteUploader::DeckCreationFailed::", err)
			}
			log.Printf("'%s' created deck\n", task.deck)
		}
		if len(task.notes) == 0 {
			log.Printf("%#v contains no cards!\n", task.deck)
		}
		i := 0
		Outer: for i < len(task.notes) {
			note := task.notes[i]
			err := client.Notes.Add(note)
			// handle response code
			switch {
				case err == nil || err.StatusCode == 200:
				case err.StatusCode == 500:
					time.Sleep(100 * time.Millisecond)
					continue Outer
				default: 
					s, _ := json.MarshalIndent(note, "", "\t")
					log.Fatalf("NoteUploader::UploadFailed:: %v \n Note: \n %v\n", err, string(s))
			}
			i++
		}
		log.Printf("'%s' added %d notes to anki\n", task.deck, len(task.notes))
	}
}


