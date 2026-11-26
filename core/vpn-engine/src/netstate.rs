//! What the service can check about the network after an engine (the managed worker or OpenVPN)
//! ended without confirming that it removed what it had set up.
//!
//! That is all the check is: whether the interface the engine created, and the addresses it
//! assigned, are still configured on this machine. It does not inspect routing rules, firewall
//! filters or DNS settings. On Windows the system removes the routes, DNS servers and filters of
//! an interface with the interface; the check is a way to see that this happened, not a proof
//! about every setting. When it cannot tell, it says so, and the caller keeps the block.
//!
//! On Linux it does not look at `ip rule` or nftables either, on purpose (decided in 0.9.2, with
//! the evidence pinned by `worker_session`'s test `the_tunnel_inbound_leaves_nothing_that_outlives_its_interface_and_cuts_traffic`):
//! the worker's TUN is not persistent, so its routes and addresses go with it; its configuration
//! has no `auto_redirect`, so there are no nftables entries; and the `ip rule`s it leaves point at
//! an empty table, which does not match, and are removed by the worker's own next start, which a
//! check that held the block for them would keep from ever happening.

use anyhow::{bail, Result};
use std::{
    collections::HashSet,
    net::{IpAddr, Ipv4Addr, Ipv6Addr},
};

/// What an engine session put in the operating system.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Footprint {
    /// The name of the network interface the engine created (the managed worker's `Ghostlink`).
    pub interface: Option<String>,
    /// The addresses the engine assigned to its interface.
    pub addresses: Vec<IpAddr>,
}

/// The network interfaces of this machine and the addresses on them, at one moment.
#[derive(Clone, Debug, Default)]
pub struct Interfaces {
    pub names: Vec<String>,
    pub addresses: HashSet<IpAddr>,
}

impl Footprint {
    /// Whether anything of it is still configured in `now`. Interface names are compared without
    /// regard to case, as Windows does.
    pub fn remains_in(&self, now: &Interfaces) -> bool {
        self.interface
            .as_deref()
            .is_some_and(|name| now.names.iter().any(|n| n.eq_ignore_ascii_case(name)))
            || self.addresses.iter().any(|a| now.addresses.contains(a))
    }

    /// Whether anything of it is still configured on this machine.
    ///
    /// # Errors
    /// Fails if the footprint names nothing to look for, or if the system cannot be asked: the
    /// answer is then unknown, which is not the same as "gone".
    pub fn remains(&self) -> Result<bool> {
        if self.interface.is_none() && self.addresses.is_empty() {
            bail!("There is nothing to look for");
        }
        Ok(self.remains_in(&interfaces()?))
    }
}

/// The interfaces and addresses of this machine now.
///
/// # Errors
/// Fails if the system cannot be asked, or on a system this is not written for.
pub fn interfaces() -> Result<Interfaces> {
    #[cfg(windows)]
    {
        windows_interfaces()
    }
    #[cfg(target_os = "linux")]
    {
        linux_interfaces()
    }
    #[cfg(not(any(windows, target_os = "linux")))]
    bail!("Network interfaces cannot be listed on this platform")
}

#[cfg(windows)]
fn windows_interfaces() -> Result<Interfaces> {
    use anyhow::ensure;
    use windows_sys::Win32::{
        Foundation::{ERROR_BUFFER_OVERFLOW, ERROR_SUCCESS},
        NetworkManagement::IpHelper::{
            GetAdaptersAddresses, GAA_FLAG_INCLUDE_ALL_INTERFACES, GAA_FLAG_SKIP_ANYCAST,
            GAA_FLAG_SKIP_DNS_SERVER, GAA_FLAG_SKIP_MULTICAST, IP_ADAPTER_ADDRESSES_LH,
        },
        Networking::WinSock::{AF_INET, AF_INET6, AF_UNSPEC, SOCKADDR_IN, SOCKADDR_IN6},
    };
    // Every interface, also those that are down or disconnected: they keep their configuration.
    let flags = GAA_FLAG_INCLUDE_ALL_INTERFACES
        | GAA_FLAG_SKIP_ANYCAST
        | GAA_FLAG_SKIP_MULTICAST
        | GAA_FLAG_SKIP_DNS_SERVER;
    let mut size = 32 * 1024u32;
    for _ in 0..5 {
        // u64 words: the structure's alignment, whatever the allocator hands out.
        let mut buffer = vec![0u64; (size as usize).div_ceil(8)];
        let first = buffer.as_mut_ptr().cast::<IP_ADAPTER_ADDRESSES_LH>();
        // SAFETY: `first` points at a zeroed buffer of `size` bytes that outlives the call, which
        // fills it with a list of adapters (and their addresses) inside that buffer, or reports
        // the size it needs.
        let status = unsafe {
            GetAdaptersAddresses(AF_UNSPEC as u32, flags, std::ptr::null(), first, &mut size)
        };
        if status == ERROR_BUFFER_OVERFLOW {
            // The list grew between the calls; `size` is now what it needs.
            continue;
        }
        ensure!(
            status == ERROR_SUCCESS,
            "Cannot list the network interfaces"
        );
        let mut found = Interfaces::default();
        // SAFETY: the call succeeded, so `first` heads a list of IP_ADAPTER_ADDRESSES_LH
        // entries inside `buffer`, each linked to the next by `Next` (null at the end), and each
        // with a list of unicast addresses linked the same way. Every pointer read below points
        // into `buffer`, which lives until the end of this block. FriendlyName is NUL-terminated
        // (the loop stops at the NUL), and a socket address is read as the structure its family
        // names.
        unsafe {
            let mut adapter = first;
            while !adapter.is_null() {
                let entry = &*adapter;
                if !entry.FriendlyName.is_null() {
                    let mut len = 0;
                    while *entry.FriendlyName.add(len) != 0 {
                        len += 1;
                    }
                    found
                        .names
                        .push(String::from_utf16_lossy(std::slice::from_raw_parts(
                            entry.FriendlyName,
                            len,
                        )));
                }
                let mut unicast = entry.FirstUnicastAddress;
                while !unicast.is_null() {
                    let address = &*unicast;
                    let socket = address.Address.lpSockaddr;
                    if !socket.is_null() {
                        match (*socket).sa_family {
                            AF_INET => {
                                let v4 = &*socket.cast::<SOCKADDR_IN>();
                                found.addresses.insert(IpAddr::V4(Ipv4Addr::from(
                                    v4.sin_addr.S_un.S_addr.to_ne_bytes(),
                                )));
                            }
                            AF_INET6 => {
                                let v6 = &*socket.cast::<SOCKADDR_IN6>();
                                found
                                    .addresses
                                    .insert(IpAddr::V6(Ipv6Addr::from(v6.sin6_addr.u.Byte)));
                            }
                            _ => {}
                        }
                    }
                    unicast = address.Next;
                }
                adapter = entry.Next;
            }
        }
        return Ok(found);
    }
    bail!("The list of network interfaces kept changing")
}

#[cfg(target_os = "linux")]
fn linux_interfaces() -> Result<Interfaces> {
    use std::fs;
    let mut found = Interfaces::default();
    for entry in fs::read_dir("/sys/class/net")? {
        found
            .names
            .push(entry?.file_name().to_string_lossy().into_owned());
    }
    found
        .addresses
        .extend(parse_fib_trie(&fs::read_to_string("/proc/net/fib_trie")?));
    // Without IPv6 support the file is not there: there are no IPv6 addresses then.
    if let Ok(text) = fs::read_to_string("/proc/net/if_inet6") {
        found.addresses.extend(parse_if_inet6(&text));
    }
    Ok(found)
}

/// The local IPv4 addresses in `/proc/net/fib_trie`: each is a `|-- ADDRESS` line followed by
/// `/32 host LOCAL`. (Read as text so it can be tested anywhere; only Linux has the file.)
#[cfg_attr(not(any(target_os = "linux", test)), allow(dead_code))]
fn parse_fib_trie(text: &str) -> HashSet<IpAddr> {
    let lines: Vec<&str> = text.lines().map(str::trim).collect();
    lines
        .windows(2)
        .filter(|pair| pair[1] == "/32 host LOCAL")
        .filter_map(|pair| pair[0].strip_prefix("|-- ")?.parse::<Ipv4Addr>().ok())
        .map(IpAddr::V4)
        .collect()
}

/// The IPv6 addresses in `/proc/net/if_inet6`: 32 hex digits, then the interface's index, prefix
/// length, scope and flags, and its name.
#[cfg_attr(not(any(target_os = "linux", test)), allow(dead_code))]
fn parse_if_inet6(text: &str) -> HashSet<IpAddr> {
    text.lines()
        .filter_map(|line| {
            let hex = line.split_whitespace().next()?;
            let bytes = u128::from_str_radix(hex, 16)
                .ok()
                .filter(|_| hex.len() == 32)?;
            Some(IpAddr::V6(Ipv6Addr::from(bytes)))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn footprint(interface: Option<&str>, addresses: &[&str]) -> Footprint {
        Footprint {
            interface: interface.map(String::from),
            addresses: addresses.iter().map(|a| a.parse().unwrap()).collect(),
        }
    }

    fn machine(names: &[&str], addresses: &[&str]) -> Interfaces {
        Interfaces {
            names: names.iter().map(|n| n.to_string()).collect(),
            addresses: addresses.iter().map(|a| a.parse().unwrap()).collect(),
        }
    }

    #[test]
    fn a_footprint_remains_while_its_interface_or_an_address_does() {
        let worker = footprint(Some("Ghostlink"), &["172.28.85.1", "fdfe:85:85::1"]);
        assert!(!worker.remains_in(&machine(&["Ethernet", "Wi-Fi"], &["192.0.2.10"])));
        // The interface alone, in any case: Windows names are not case-sensitive.
        assert!(worker.remains_in(&machine(&["Ethernet", "GHOSTLINK"], &[])));
        // An address alone, on whatever interface.
        assert!(worker.remains_in(&machine(&["Ethernet"], &["172.28.85.1"])));
        assert!(worker.remains_in(&machine(&["Ethernet"], &["fdfe:85:85::1"])));
        let openvpn = footprint(None, &["198.51.100.20"]);
        assert!(!openvpn.remains_in(&machine(&["Ghostlink"], &["172.28.85.1"])));
        assert!(openvpn.remains_in(&machine(&["tun0"], &["198.51.100.20"])));
    }

    /// The real system: this machine has a loopback address and no interface called like this.
    #[test]
    fn the_real_interfaces_are_listed_and_compared() {
        let now = interfaces().unwrap();
        assert!(now.addresses.contains(&"127.0.0.1".parse().unwrap()));
        assert!(footprint(None, &["127.0.0.1"]).remains().unwrap());
        assert!(!footprint(Some("Ghostlink-test-none"), &["203.0.113.250"])
            .remains()
            .unwrap());
    }

    #[test]
    fn a_footprint_that_names_nothing_cannot_be_checked() {
        assert!(Footprint::default().remains().is_err());
    }

    #[test]
    fn linux_address_lists_are_read() {
        let trie = "Main:\n  +-- 0.0.0.0/0 3 0 5\n     |-- 0.0.0.0\n        /0 universe UNICAST\n     +-- 127.0.0.0/8 2 0 2\n        +-- 127.0.0.0/31 1 0 0\n           |-- 127.0.0.0\n              /32 link BROADCAST\n              /8 host LOCAL\n           |-- 127.0.0.1\n              /32 host LOCAL\n        |-- 127.255.255.255\n           /32 link BROADCAST\n     +-- 172.28.85.0/30 2 0 2\n        |-- 172.28.85.0\n           /32 link BROADCAST\n        |-- 172.28.85.1\n           /32 host LOCAL\nLocal:\n  +-- 0.0.0.0/0 3 0 5\n     |-- 192.0.2.10\n        /32 host LOCAL\n";
        let found = parse_fib_trie(trie);
        let expected: HashSet<IpAddr> = ["127.0.0.1", "172.28.85.1", "192.0.2.10"]
            .iter()
            .map(|a| a.parse().unwrap())
            .collect();
        assert_eq!(found, expected);
        let inet6 = "00000000000000000000000000000001 01 80 10 80       lo\nfdfe0085008500000000000000000001 07 7e 00 80 Ghostlink\nnot hex at all 00 00 00 00 junk\n";
        let found = parse_if_inet6(inet6);
        let expected: HashSet<IpAddr> = ["::1", "fdfe:85:85::1"]
            .iter()
            .map(|a| a.parse().unwrap())
            .collect();
        assert_eq!(found, expected);
    }
}
