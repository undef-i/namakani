package avatar

import (
	"bytes"
	"encoding/xml"
	"strings"
)

var svgElements = map[string]bool{
	"svg": true, "g": true, "defs": true, "title": true, "symbol": true,
	"path": true, "rect": true, "circle": true, "ellipse": true,
	"line": true, "polyline": true, "polygon": true,
	"linearGradient": true, "radialGradient": true, "stop": true,
	"use": true, "clipPath": true,
}

var svgDrawable = map[string]bool{
	"path": true, "rect": true, "circle": true, "ellipse": true,
	"line": true, "polyline": true, "polygon": true, "use": true,
}

var svgAttrs = map[string]bool{
	"id": true, "fill": true, "fill-opacity": true, "fill-rule": true,
	"stroke": true, "stroke-width": true, "stroke-opacity": true,
	"stroke-linecap": true, "stroke-linejoin": true, "stroke-miterlimit": true,
	"stroke-dasharray": true, "stroke-dashoffset": true,
	"opacity": true, "transform": true, "viewBox": true,
	"width": true, "height": true, "x": true, "y": true,
	"x1": true, "y1": true, "x2": true, "y2": true,
	"cx": true, "cy": true, "r": true, "rx": true, "ry": true,
	"points": true, "d": true, "offset": true,
	"stop-color": true, "stop-opacity": true,
	"gradientUnits": true, "gradientTransform": true, "spreadMethod": true,
	"clip-rule": true, "preserveAspectRatio": true, "version": true,
	"color": true, "display": true, "visibility": true,
}

func attrAllowed(name xml.Name, val string) bool {
	local := name.Local
	if name.Space == "xmlns" || local == "xmlns" {
		return true
	}
	if local == "href" {
		return strings.HasPrefix(val, "#")
	}
	if !svgAttrs[local] {
		return false
	}
	lv := strings.ToLower(val)
	if strings.Contains(lv, "url(") && !strings.Contains(lv, "url(#") {
		return false
	}
	if strings.Contains(lv, "http") || strings.Contains(lv, "javascript:") || strings.Contains(lv, "data:") {
		return false
	}
	if local == "clip-path" && !strings.HasPrefix(val, "url(#") {
		return false
	}
	return true
}

func SanitizeSVG(in []byte) ([]byte, bool) {
	if len(in) == 0 || len(in) > 512<<10 {
		return nil, false
	}
	dec := xml.NewDecoder(bytes.NewReader(in))
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)
	sawRoot := false
	drawable := false
	var stack []string
	skipDepth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if skipDepth > 0 {
				skipDepth++
				continue
			}
			if !svgElements[name] {
				skipDepth = 1
				continue
			}
			if !sawRoot {
				if name != "svg" {
					return nil, false
				}
				sawRoot = true
			}
			if svgDrawable[name] {
				drawable = true
			}
			out := xml.StartElement{Name: xml.Name{Local: name}}
			hasXMLNS := false
			for _, a := range t.Attr {
				if a.Name.Local == "xmlns" || a.Name.Space == "xmlns" {
					hasXMLNS = true
				}
				if attrAllowed(a.Name, a.Value) {
					out.Attr = append(out.Attr, xml.Attr{Name: xml.Name{Local: a.Name.Local}, Value: a.Value})
				}
			}
			if name == "svg" && !hasXMLNS {
				out.Attr = append(out.Attr, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: "http://www.w3.org/2000/svg"})
			}
			if err := enc.EncodeToken(out); err != nil {
				return nil, false
			}
			stack = append(stack, name)
		case xml.EndElement:
			if skipDepth > 0 {
				skipDepth--
				continue
			}
			name := t.Name.Local
			if len(stack) > 0 && stack[len(stack)-1] == name {
				stack = stack[:len(stack)-1]
				if err := enc.EncodeToken(xml.EndElement{Name: xml.Name{Local: name}}); err != nil {
					return nil, false
				}
			}
		case xml.CharData:
			if skipDepth == 0 && len(stack) > 0 && stack[len(stack)-1] == "title" {
				_ = enc.EncodeToken(xml.CharData(t.Copy()))
			}
		}
	}
	if err := enc.Flush(); err != nil {
		return nil, false
	}
	if !sawRoot || !drawable {
		return nil, false
	}
	return buf.Bytes(), true
}
