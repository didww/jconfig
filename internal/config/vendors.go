package config

import "sort"

// Vendor names.
const (
	VendorJunos    = "junos"
	VendorRouterOS = "routeros"
)

// Config formats that can be fetched from a Junos device.
const (
	FormatText = "text"
	FormatSet  = "set"
	FormatXML  = "xml"
)

// Config formats that can be fetched from a RouterOS device. They are the
// renderings of `/export`: compact (RouterOS' own default, non-default
// settings only), verbose (every setting, defaults included) and terse (one
// line per item, for machine processing).
const (
	FormatExport  = "export"
	FormatVerbose = "verbose"
	FormatTerse   = "terse"
)

// formatInfo describes one config rendering.
type formatInfo struct {
	// ext is the file extension it is stored under.
	ext string
	// commented reports whether "#" starts a comment in this rendering, and
	// so whether the metadata header can be prefixed to it.
	commented bool
}

// vendorInfo is what a vendor's driver can be asked for. It lives here rather
// than in the driver packages so that `-check` can validate an inventory
// without linking a transport.
type vendorInfo struct {
	transports     []string
	formats        map[string]formatInfo
	defaultFormats []string
	// defaultPorts maps a transport to the port used when none is set.
	defaultPorts map[string]int
}

var vendors = map[string]vendorInfo{
	VendorJunos: {
		transports: []string{TransportSSH, TransportNETCONF},
		formats: map[string]formatInfo{
			FormatText: {ext: ".conf", commented: true},
			FormatSet:  {ext: ".set", commented: true},
			// "#" is not a comment in XML: prefixing the header would
			// produce a malformed document.
			FormatXML: {ext: ".xml", commented: false},
		},
		defaultFormats: []string{FormatText, FormatSet},
		defaultPorts:   map[string]int{TransportSSH: 22, TransportNETCONF: 830},
	},
	VendorRouterOS: {
		// RouterOS has no NETCONF; everything goes over the SSH CLI.
		transports: []string{TransportSSH},
		formats: map[string]formatInfo{
			FormatExport:  {ext: ".rsc", commented: true},
			FormatVerbose: {ext: ".verbose.rsc", commented: true},
			FormatTerse:   {ext: ".terse.rsc", commented: true},
		},
		defaultFormats: []string{FormatExport},
		defaultPorts:   map[string]int{TransportSSH: 22},
	},
}

// Vendors returns every known vendor name, sorted.
func Vendors() []string { return sortedKeys(vendors) }

// KnownVendor reports whether the vendor has a driver.
func KnownVendor(vendor string) bool { _, ok := vendors[vendor]; return ok }

// Transports returns the transports the vendor supports, sorted.
func Transports(vendor string) []string {
	v, ok := vendors[vendor]
	if !ok {
		return nil
	}
	return v.transports
}

// Formats returns the config formats the vendor can render, sorted.
func Formats(vendor string) []string {
	v, ok := vendors[vendor]
	if !ok {
		return nil
	}
	return sortedKeys(v.formats)
}

// CommentedFormats returns the vendor's formats in which "#" is a comment, so
// the metadata header can be prefixed to them. Sorted, so the header lands in
// a deterministic order.
func CommentedFormats(vendor string) []string {
	v, ok := vendors[vendor]
	if !ok {
		return nil
	}
	var out []string
	for name, f := range v.formats {
		if f.commented {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Extension is the file extension a format is stored under, empty when the
// vendor cannot render it.
func Extension(vendor, format string) string {
	v, ok := vendors[vendor]
	if !ok {
		return ""
	}
	return v.formats[format].ext
}

// Extension is the file extension this device's format is stored under.
func (d *Device) Extension(format string) string { return Extension(d.Vendor, format) }

func supports(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
