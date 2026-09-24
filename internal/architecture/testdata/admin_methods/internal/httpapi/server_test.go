package httpapi

// A test calling a method does not keep it in use.
func useInTest(s *Server) { s.admin.Unused() }
