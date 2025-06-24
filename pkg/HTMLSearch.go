package HTMLTrees

import (
	"log"
	"regexp"

	"golang.org/x/net/html"
)

// returns first textnode  from `root`'s subtree matching `regex` or nil if no textnode is found.
func MatchingNodes(root *html.Node, regex *regexp.Regexp) []*html.Node {
	switch root.Type {
	case html.TextNode:
		match := regex.MatchString(root.Data)
		//log.Printf("Pattern : %#v ; %#v ; %v\n", regex.String(), root.Data, match)
		if match {
			return []*html.Node{root}
		}
		return nil

	case html.ElementNode: 
		res := make([]*html.Node, 0)
		for c := root.FirstChild; c != nil; c = c.NextSibling {
			t := MatchingNodes(c, regex)
			if t != nil {
				res = append(res, t...)
			}

		}
		return res
	default:
		log.Fatal(root.Type)
	}
	return nil
}

