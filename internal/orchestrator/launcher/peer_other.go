// SPDX-FileCopyrightText: Copyright (C) Arduino s.r.l. and/or its affiliated companies
//
// SPDX-License-Identifier: MPL-2.0

//go:build !linux

package launcher

import "net"

// socketPeerPID cannot tell the peer here: the daemon runs on Linux boards, this is development only.
func socketPeerPID(net.Conn) (int, error) {
	return 0, nil
}
