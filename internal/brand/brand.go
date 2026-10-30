// Package brand holds the product's name in one place, so renaming it is
// a one-line change. Code names (environment variables, cookie, binary)
// derive from Code.
package brand

// Name is what people see: the header, the page title, the assistant's
// introduction.
var Name = "MyMedical"

// Code is the short machine name: MYMEDICAL_* settings, the cookie.
var Code = "mymedical"
