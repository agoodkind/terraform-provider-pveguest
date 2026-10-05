// Package sysrepo parses and builds the text that the sysrepo command-line
// tools read and write.
package sysrepo

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// ErrMalformedXML marks content that is not a well-formed XML document.
var ErrMalformedXML = errors.New("malformed XML")

const (
	xmlWhitespace = " \t\r\n"
	namespaceAttr = "xmlns"
)

// CanonicalXML returns the canonical form of an XML document. Two documents
// with the same data have the same canonical form.
//
// The canonical form has one line per token of the Go encoding/xml token
// stream. It keeps element names with their namespace URIs, attributes sorted
// by namespace URI and name, and text. It drops comments, processing
// instructions, directives, namespace declarations, prefixes, whitespace around
// text, and text that is only whitespace. A document with several top-level
// elements is valid, and an empty document returns an empty string.
func CanonicalXML(content string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(content))
	var canonical strings.Builder
	var pendingText strings.Builder
	depth := 0

	flushText := func() error {
		text := strings.Trim(pendingText.String(), xmlWhitespace)
		pendingText.Reset()
		if text == "" {
			return nil
		}
		if depth == 0 {
			return fmt.Errorf("%w: text outside the top-level elements", ErrMalformedXML)
		}
		canonical.WriteString("text " + strconv.Quote(text) + "\n")
		return nil
	}

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			slog.Error("parse XML content failed", "err", err)
			return "", fmt.Errorf("%w: %w", ErrMalformedXML, err)
		}
		switch typed := token.(type) {
		case xml.StartElement:
			if err := flushText(); err != nil {
				return "", err
			}
			writeStartElement(&canonical, typed)
			depth++
		case xml.EndElement:
			if err := flushText(); err != nil {
				return "", err
			}
			depth--
			canonical.WriteString("end\n")
		case xml.CharData:
			pendingText.Write(typed)
		}
	}
	if err := flushText(); err != nil {
		return "", err
	}
	return canonical.String(), nil
}

func writeStartElement(canonical *strings.Builder, element xml.StartElement) {
	attributes := make([]xml.Attr, 0, len(element.Attr))
	for _, attribute := range element.Attr {
		isDeclaration := attribute.Name.Space == namespaceAttr ||
			(attribute.Name.Space == "" && attribute.Name.Local == namespaceAttr)
		if !isDeclaration {
			attributes = append(attributes, attribute)
		}
	}
	sort.Slice(attributes, func(i int, j int) bool {
		left := attributes[i].Name
		right := attributes[j].Name
		if left.Space != right.Space {
			return left.Space < right.Space
		}
		return left.Local < right.Local
	})

	fmt.Fprintf(canonical, "start {%s}%s", element.Name.Space, element.Name.Local)
	for _, attribute := range attributes {
		fmt.Fprintf(
			canonical, " {%s}%s=%s",
			attribute.Name.Space, attribute.Name.Local, strconv.Quote(attribute.Value),
		)
	}
	canonical.WriteString("\n")
}
