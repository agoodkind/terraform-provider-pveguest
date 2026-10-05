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

var (
	// ErrMalformedXML marks content that is not a well-formed XML document.
	ErrMalformedXML = errors.New("malformed XML")
	// ErrNoElement marks content without any XML element.
	ErrNoElement = errors.New("the XML content has no element")
)

const (
	netconfNamespace = "urn:ietf:params:xml:ns:netconf:base:1.0"
	operationPrefix  = "pveguestnc"
	xmlWhitespace    = " \t\r\n"
	selfClosingTail  = "/>"
	namespaceAttr    = "xmlns"
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

// RemoveEdit returns the document with the NETCONF operation "remove" on each
// top-level element. A sysrepocfg edit of the result deletes the data that the
// top-level elements select. The edit succeeds when that data is absent.
func RemoveEdit(content string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(content))
	var edit strings.Builder
	copied := 0
	depth := 0
	topLevelCount := 0
	attributes := fmt.Sprintf(
		` xmlns:%s=%q %s:operation="remove"`, operationPrefix, netconfNamespace, operationPrefix,
	)

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			slog.Error("parse XML content failed", "err", err)
			return "", fmt.Errorf("%w: %w", ErrMalformedXML, err)
		}
		switch token.(type) {
		case xml.StartElement:
			if depth == 0 {
				topLevelCount++
				// InputOffset is the end of the start tag, just after the ">".
				tagEnd := int(decoder.InputOffset())
				insertAt := tagEnd - 1
				if strings.HasSuffix(content[:tagEnd], selfClosingTail) {
					insertAt--
				}
				edit.WriteString(content[copied:insertAt])
				edit.WriteString(attributes)
				copied = insertAt
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	if topLevelCount == 0 {
		return "", ErrNoElement
	}
	edit.WriteString(content[copied:])
	return edit.String(), nil
}
