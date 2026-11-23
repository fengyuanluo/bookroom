package library

import "strings"

// NormalizeISBN validates an ISBN-10 or ISBN-13 (hyphens and spaces allowed) and returns it as ISBN-13.
func NormalizeISBN(s string) (string, bool) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == 'x' || r == 'X':
			b.WriteByte('X')
		case r == '-' || r == ' ':
		default:
			return "", false
		}
	}
	d := b.String()
	switch len(d) {
	case 13:
		if (strings.HasPrefix(d, "978") || strings.HasPrefix(d, "979")) && !strings.Contains(d, "X") && isbn13Check(d[:12]) == d[12] {
			return d, true
		}
	case 10:
		if !strings.Contains(d[:9], "X") && isbn10Check(d[:9]) == d[9] {
			core := "978" + d[:9]
			return core + string(isbn13Check(core)), true
		}
	}
	return "", false
}

func isbn13Check(first12 string) byte {
	sum := 0
	for i := range 12 {
		w := 1
		if i%2 == 1 {
			w = 3
		}
		sum += int(first12[i]-'0') * w
	}
	return byte('0' + (10-sum%10)%10)
}

func isbn10Check(first9 string) byte {
	sum := 0
	for i := range 9 {
		sum += int(first9[i]-'0') * (10 - i)
	}
	c := (11 - sum%11) % 11
	if c == 10 {
		return 'X'
	}
	return byte('0' + c)
}
