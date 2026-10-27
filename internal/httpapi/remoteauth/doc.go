// Package remoteauth guards Nano's protected remote HTTP listener and owns
// its password and browser-session store. It depends on the HTTP transport
// conventions and filesystem path utilities; only the program edge may use it.
package remoteauth
