# WenzWork CrossPTY fork

This directory is based on `github.com/Kodecable/crosspty` v1.1.0
(`0dbd5253c95baefd8c3e53cb0be1d44200884448`) under its BSD 3-Clause license.

WenzWork adds `CommandConfig.WindowsDesktop`, which maps to the Windows
`STARTUPINFO.lpDesktop` field. Device Agent uses it to run remote interactive
terminals on a private, non-interactive desktop so console windows cannot flash
on the signed-in user's desktop. The Unix implementation is unchanged.
