package nfo

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxWriteSize = maxSize

var providerTags = map[string]string{
	"imdb": "imdbid", "tmdb": "tmdbid", "tvdb": "tvdbid", "anidb": "anidbid",
}

// Write updates recognized direct-child fields and provider IDs while keeping other XML tokens intact.
// fields may contain title, originaltitle, sorttitle, plot, premiered, year, season, or episode.
func Write(path, root string, fields map[string]string, providerIDs map[string]string, clearProviderIDs []string) error {
	if path == "" || root == "" {
		return errors.New("NFO path and root element are required")
	}
	oldInfo, statErr := os.Lstat(path)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if exists && (!oldInfo.Mode().IsRegular() || oldInfo.Mode()&os.ModeSymlink != 0) {
		return errors.New("NFO destination is not a regular file")
	}
	mode := os.FileMode(0o644)
	var raw []byte
	if exists {
		mode = oldInfo.Mode().Perm()
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		raw, err = io.ReadAll(io.LimitReader(file, maxWriteSize+1))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(raw) > maxWriteSize {
			return fmt.Errorf("NFO exceeds the %d byte limit", maxWriteSize)
		}
	}
	updated, err := patchXML(raw, root, fields, providerIDs, clearProviderIDs)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".flixer-nfo-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(updated); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if exists {
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(oldInfo, current) || !current.ModTime().Equal(oldInfo.ModTime()) || current.Size() != oldInfo.Size() {
			return errors.New("NFO changed while it was being updated")
		}
		return os.Rename(tempPath, path)
	}
	// Do not overwrite an NFO created by another process after the initial check.
	if err := os.Link(tempPath, path); err != nil {
		return err
	}
	return nil
}

func patchXML(raw []byte, root string, fields map[string]string, providerIDs map[string]string, clearProviderIDs []string) ([]byte, error) {
	fieldValues := map[string]string{}
	allowedFields := map[string]bool{"title": true, "originaltitle": true, "sorttitle": true, "plot": true, "premiered": true, "year": true, "season": true, "episode": true}
	for key, value := range fields {
		key = strings.ToLower(key)
		if !allowedFields[key] {
			return nil, fmt.Errorf("unsupported NFO field %q", key)
		}
		fieldValues[key] = value
	}
	idValues := map[string]string{}
	for provider, value := range providerIDs {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" || provider == "tmdbset" || len(provider) > 64 {
			continue
		}
		idValues[provider] = strings.TrimSpace(value)
	}
	for _, provider := range clearProviderIDs {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider != "" && provider != "tmdbset" && len(provider) <= 64 {
			if _, present := idValues[provider]; !present {
				idValues[provider] = ""
			}
		}
	}

	if len(raw) == 0 {
		return encodeNew(root, fieldValues, idValues)
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.Strict = false
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	tokens := make([]xml.Token, 0, 256)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse NFO: %w", err)
		}
		tokens = append(tokens, xml.CopyToken(token))
	}
	if len(tokens) == 0 {
		return nil, errors.New("NFO has no XML root element")
	}
	seenFields := map[string]bool{}
	seenIDs := map[string]bool{}
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	depth := 0
	rootFound := false
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				rootFound = true
			}
			if depth == 1 {
				key := strings.ToLower(value.Name.Local)
				if _, present := fieldValues[key]; present {
					seenFields[key] = true
					end := matchingElementEnd(tokens, index)
					if fieldValues[key] != "" {
						if err := encodeTextElement(encoder, value, fieldValues[key]); err != nil {
							return nil, err
						}
					}
					index = end
					continue
				}
				if provider, ok := providerForElement(value); ok {
					if text, present := idValues[provider]; present {
						seenIDs[provider] = true
						end := matchingElementEnd(tokens, index)
						if text != "" {
							if err := encodeTextElement(encoder, value, text); err != nil {
								return nil, err
							}
						}
						index = end
						continue
					}
				}
			}
			depth++
		case xml.EndElement:
			if depth == 1 && rootFound {
				if err := appendMissing(encoder, fieldValues, seenFields, idValues, seenIDs); err != nil {
					return nil, err
				}
			}
			depth--
		}
		if err := encoder.EncodeToken(token); err != nil {
			return nil, err
		}
	}
	if !rootFound || depth != 0 {
		return nil, errors.New("NFO XML has an invalid root structure")
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func matchingElementEnd(tokens []xml.Token, start int) int {
	depth := 0
	for index := start; index < len(tokens); index++ {
		switch tokens[index].(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return len(tokens) - 1
}

func providerForElement(element xml.StartElement) (string, bool) {
	if strings.EqualFold(element.Name.Local, "uniqueid") {
		for _, attribute := range element.Attr {
			if strings.EqualFold(attribute.Name.Local, "type") {
				provider := strings.ToLower(strings.TrimSpace(attribute.Value))
				return provider, provider != ""
			}
		}
		return "", false
	}
	for provider, tag := range providerTags {
		if strings.EqualFold(element.Name.Local, tag) {
			return provider, true
		}
	}
	return "", false
}

func encodeTextElement(encoder *xml.Encoder, start xml.StartElement, value string) error {
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	if err := encoder.EncodeToken(xml.CharData(value)); err != nil {
		return err
	}
	return encoder.EncodeToken(start.End())
}

func appendMissing(encoder *xml.Encoder, fields map[string]string, seenFields map[string]bool, ids map[string]string, seenIDs map[string]bool) error {
	fieldOrder := []string{"title", "originaltitle", "sorttitle", "year", "premiered", "plot", "season", "episode"}
	for _, key := range fieldOrder {
		value, present := fields[key]
		if !present || seenFields[key] || value == "" {
			continue
		}
		if err := encodeTextElement(encoder, xml.StartElement{Name: xml.Name{Local: key}}, value); err != nil {
			return err
		}
	}
	providers := make([]string, 0, len(ids))
	for provider, value := range ids {
		if value != "" && !seenIDs[provider] {
			providers = append(providers, provider)
		}
	}
	sort.Strings(providers)
	for _, provider := range providers {
		start := xml.StartElement{Name: xml.Name{Local: "uniqueid"}, Attr: []xml.Attr{{Name: xml.Name{Local: "type"}, Value: provider}}}
		if err := encodeTextElement(encoder, start, ids[provider]); err != nil {
			return err
		}
	}
	return nil
}

func encodeNew(root string, fields map[string]string, ids map[string]string) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString(xml.Header)
	encoder := xml.NewEncoder(&output)
	start := xml.StartElement{Name: xml.Name{Local: root}}
	if err := encoder.EncodeToken(start); err != nil {
		return nil, err
	}
	if err := appendMissing(encoder, fields, map[string]bool{}, ids, map[string]bool{}); err != nil {
		return nil, err
	}
	if err := encoder.EncodeToken(start.End()); err != nil {
		return nil, err
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
