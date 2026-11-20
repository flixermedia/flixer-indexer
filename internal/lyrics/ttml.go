package lyrics

import (
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var lrcTimestamp = regexp.MustCompile(`\[\d{1,3}:\d{1,2}(?:[.:]\d{1,3})?\]`)

type timedParagraph struct {
	start float64
	end   float64
	text  string
}

func ttmlSeconds(value string) (float64, error) {
	value = strings.TrimSpace(value)
	for _, unit := range []struct {
		suffix string
		scale  float64
	}{{"ms", 0.001}, {"s", 1}, {"m", 60}, {"h", 3600}} {
		if strings.HasSuffix(value, unit.suffix) {
			amount, err := strconv.ParseFloat(strings.TrimSuffix(value, unit.suffix), 64)
			if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
				return 0, fmt.Errorf("invalid timestamp %q", value)
			}
			return amount * unit.scale, nil
		}
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("invalid timestamp %q", value)
	}
	seconds := 0.0
	for _, part := range parts {
		amount, err := strconv.ParseFloat(part, 64)
		if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
			return 0, fmt.Errorf("invalid timestamp %q", value)
		}
		seconds = seconds*60 + amount
	}
	return seconds, nil
}

func lrcTime(seconds float64) string {
	milliseconds := int64(math.Round(seconds * 1000))
	return fmt.Sprintf("%02d:%02d.%03d", milliseconds/60000, milliseconds/1000%60, milliseconds%1000)
}

func xmlAttribute(element xml.StartElement, name string) string {
	for _, attribute := range element.Attr {
		if attribute.Name.Local == name {
			return attribute.Value
		}
	}
	return ""
}

func readTTMLParagraph(decoder *xml.Decoder, element xml.StartElement) (timedParagraph, error) {
	paragraph := timedParagraph{start: -1}
	var err error
	if begin := xmlAttribute(element, "begin"); begin != "" {
		paragraph.start, err = ttmlSeconds(begin)
		if err != nil {
			return paragraph, err
		}
	}
	if end := xmlAttribute(element, "end"); end != "" {
		paragraph.end, err = ttmlSeconds(end)
		if err != nil {
			return paragraph, err
		}
	}
	var text strings.Builder
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return paragraph, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			role := xmlAttribute(token, "role")
			if role == "x-bg" || role == "x-translation" || role == "x-roman" {
				if err := decoder.Skip(); err != nil {
					return paragraph, err
				}
				continue
			}
			depth++
			if begin := xmlAttribute(token, "begin"); begin != "" {
				start, err := ttmlSeconds(begin)
				if err != nil {
					return paragraph, err
				}
				if paragraph.start < 0 {
					paragraph.start = start
				}
				text.WriteString("<" + lrcTime(start) + ">")
			}
			if token.Name.Local == "br" {
				text.WriteByte(' ')
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			value := string(token)
			if strings.ContainsAny(value, "\r\n\t") {
				value = strings.TrimSpace(value)
			}
			text.WriteString(value)
		}
	}
	paragraph.text = strings.TrimSpace(text.String())
	return paragraph, nil
}

func ttmlToLRC(source string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(source))
	var paragraphs []timedParagraph
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if element, ok := token.(xml.StartElement); ok && element.Name.Local == "p" {
			paragraph, err := readTTMLParagraph(decoder, element)
			if err != nil {
				return "", err
			}
			if paragraph.start >= 0 && paragraph.text != "" {
				paragraphs = append(paragraphs, paragraph)
			}
		}
	}
	if len(paragraphs) == 0 {
		return "", ErrNotFound
	}
	sort.SliceStable(paragraphs, func(left, right int) bool { return paragraphs[left].start < paragraphs[right].start })
	var result strings.Builder
	for index, paragraph := range paragraphs {
		fmt.Fprintf(&result, "[%s]%s\n", lrcTime(paragraph.start), paragraph.text)
		if paragraph.end > paragraph.start && (index+1 == len(paragraphs) || paragraphs[index+1].start > paragraph.end) {
			fmt.Fprintf(&result, "[%s]\n", lrcTime(paragraph.end))
		}
	}
	return result.String(), nil
}
