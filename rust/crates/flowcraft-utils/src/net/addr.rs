//! The address model host patterns need: Go's `net/netip`, cut down.
//!
//! Two types, because core's matching asks exactly two questions of an
//! address: is it this one ([`Addr`] equality), and does it fall inside this
//! range ([`Prefix::contains`]). Neither type renders an address -- nothing in
//! the policy path prints one -- so what is here is parsing and comparison.
//!
//! # The three spellings that decide the kind
//!
//! `netip.ParseAddr` dispatches on the *first* `.`, `:` or `%` in the text,
//! and so does [`Addr::parse`]:
//!
//! - `.` first: an IPv4 address, and nothing else. `1.2.3.4%eth0` is an error
//!   here, not a zoned IPv4.
//! - `:` first: an IPv6 address, whose zone (after the first `%`) must be
//!   non-empty if it is there at all.
//! - `%` first: no address precedes the zone, so the text is not an address.
//!
//! What that decides in practice is fall-through: a string core does not read
//! as an address becomes a *hostname* pattern, so `10.0.0.0/33` matches the
//! name `10.0.0.0/33` and no address at all. `rust/conformance/net-contract.json`
//! pins each of those spellings.
//!
//! # IPv4-mapped addresses
//!
//! `::ffff:1.2.3.4` is an IPv6 address with IPv6's 128-bit width, and
//! [`Addr::unmap`] turns it into the IPv4 address it wraps -- core's `Unmap`,
//! and the reason `10.0.0.0/8` matches a client that dialed through the mapped
//! form while `::/0` does not. It is also why a prefix written in the mapped
//! form (`::ffff:10.0.0.0/104`) matches nothing: by the time a prefix is
//! consulted the query has been unmapped, and 32 bits never satisfy a 104-bit
//! prefix.

use std::net::{Ipv4Addr, Ipv6Addr};

/// A parsed address, IPv4 or IPv6.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Addr {
    /// An IPv4 address.
    V4(Ipv4Addr),
    /// An IPv6 address, with the zone it was written with if it had one.
    ///
    /// A zone never matches a prefix (`fe80::1%eth0` is not inside
    /// `fe80::/10` for core, and `contains` says so) but it does make two
    /// addresses unequal, which is why it is part of the value.
    V6 {
        /// The address.
        addr: Ipv6Addr,
        /// The scope id, as written after `%`.
        zone: Option<Box<str>>,
    },
}

impl Addr {
    /// `netip.ParseAddr`, for the inputs a host pattern can carry.
    ///
    /// Returns `None` where Go returns an error; the caller treats both as
    /// "not an address", which is the only thing the decision depends on.
    pub fn parse(text: &str) -> Option<Addr> {
        match text.bytes().find(|byte| matches!(byte, b'.' | b':' | b'%')) {
            Some(b'.') => text.parse::<Ipv4Addr>().ok().map(Addr::V4),
            Some(b':') => {
                let (addr, zone) = match text.split_once('%') {
                    Some((addr, zone)) => {
                        // An explicitly empty zone is refused, not ignored.
                        if zone.is_empty() {
                            return None;
                        }
                        (addr, Some(zone))
                    }
                    None => (text, None),
                };
                addr.parse::<Ipv6Addr>().ok().map(|addr| Addr::V6 {
                    addr,
                    zone: zone.map(Box::from),
                })
            }
            // A zone before any address, or no separator at all.
            Some(_) | None => None,
        }
    }

    /// `Addr.Unmap`: the IPv4 address an IPv4-mapped IPv6 address wraps, and
    /// everything else unchanged.
    ///
    /// The zone goes with it, like core's: the unmapped value is an IPv4
    /// address, and IPv4 addresses have no zones.
    pub fn unmap(&self) -> Addr {
        match self {
            Addr::V6 { addr, .. } => match addr.to_ipv4_mapped() {
                Some(addr) => Addr::V4(addr),
                None => self.clone(),
            },
            Addr::V4(_) => self.clone(),
        }
    }

    /// `Addr.BitLen`: 32 for an IPv4 address, 128 for anything IPv6 --
    /// including the mapped form, which is the whole point of unmapping first.
    pub fn bit_len(&self) -> u8 {
        match self {
            Addr::V4(_) => 32,
            Addr::V6 { .. } => 128,
        }
    }

    /// The zone, if the address was written with one.
    pub fn zone(&self) -> Option<&str> {
        match self {
            Addr::V4(_) => None,
            Addr::V6 { zone, .. } => zone.as_deref(),
        }
    }
}

/// A CIDR range, stored with its host bits cleared (core's `Prefix.Masked()`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Prefix {
    addr: Addr,
    bits: u8,
}

impl Prefix {
    /// `netip.ParsePrefix`: an address, a `/`, and a bit count within the
    /// address's width.
    ///
    /// A zoned address is refused, as core refuses it: a range is not scoped.
    pub fn parse(text: &str) -> Option<Prefix> {
        let (addr, bits) = text.split_once('/')?;
        if addr.is_empty() || bits.is_empty() {
            return None;
        }
        let addr = Addr::parse(addr)?;
        if addr.zone().is_some() {
            return None;
        }
        let bits = bits.parse::<i64>().ok()?;
        if bits < 0 || bits > i64::from(addr.bit_len()) {
            return None;
        }
        let bits = u8::try_from(bits).ok()?;
        Some(Prefix {
            addr: mask(&addr, bits),
            bits,
        })
    }

    /// The address the range starts at.
    pub fn addr(&self) -> &Addr {
        &self.addr
    }

    /// The number of leading bits that are fixed.
    pub fn bits(&self) -> u8 {
        self.bits
    }

    /// `Prefix.Contains`: is `ip` inside the range?
    ///
    /// Widths must agree, so a 32-bit address is never inside a 128-bit range
    /// (and after [`Addr::unmap`], a mapped query is 32 bits). A zoned address
    /// is inside nothing.
    pub fn contains(&self, ip: &Addr) -> bool {
        match (&self.addr, ip) {
            (Addr::V4(net), Addr::V4(ip)) => {
                let shared = u32::from(*net) ^ u32::from(*ip);
                self.bits == 0 || shared >> (32 - self.bits) == 0
            }
            (Addr::V6 { addr: net, .. }, Addr::V6 { addr: ip, zone }) => {
                if zone.is_some() {
                    return false;
                }
                let full = usize::from(self.bits / 8);
                let net = net.octets();
                let ip = ip.octets();
                if net[..full] != ip[..full] {
                    return false;
                }
                let rest = self.bits % 8;
                if rest == 0 {
                    return true;
                }
                let mask = 0xFFu8 << (8 - rest);
                net[full] & mask == ip[full] & mask
            }
            _ => false,
        }
    }
}

/// Clear everything after the first `bits` bits.
fn mask(addr: &Addr, bits: u8) -> Addr {
    match addr {
        Addr::V4(addr) => {
            let mask = if bits == 0 {
                0
            } else {
                u32::MAX << (32 - bits)
            };
            Addr::V4(Ipv4Addr::from(u32::from(*addr) & mask))
        }
        Addr::V6 { addr, zone } => {
            let mut octets = addr.octets();
            let full = usize::from(bits / 8);
            let rest = bits % 8;
            if rest == 0 {
                for octet in &mut octets[full..] {
                    *octet = 0;
                }
            } else {
                octets[full] &= 0xFFu8 << (8 - rest);
                for octet in &mut octets[full + 1..] {
                    *octet = 0;
                }
            }
            Addr::V6 {
                addr: Ipv6Addr::from(octets),
                zone: zone.clone(),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn addr(text: &str) -> Addr {
        match Addr::parse(text) {
            Some(addr) => addr,
            None => panic!("{text:?} should parse"),
        }
    }

    #[test]
    fn the_first_separator_decides_the_kind() {
        assert!(matches!(addr("1.2.3.4"), Addr::V4(_)));
        assert!(matches!(addr("::ffff:1.2.3.4"), Addr::V6 { .. }));
        // A zone on an IPv4 spelling is not an address, and neither is a zone
        // with nothing before it.
        assert_eq!(Addr::parse("1.2.3.4%eth0"), None);
        assert_eq!(Addr::parse("%eth0"), None);
        assert_eq!(Addr::parse("fe80::1%"), None);
        assert_eq!(Addr::parse(""), None);
        // The strictness Go has and a lenient parser would not.
        assert_eq!(Addr::parse("010.1.2.3"), None);
        assert_eq!(Addr::parse("1.2.3"), None);
        assert_eq!(Addr::parse("1.2.3.4.5"), None);
        assert_eq!(Addr::parse("::1::2"), None);
    }

    #[test]
    fn unmapping_makes_the_wrapped_address_ipv4() {
        let mapped = addr("::ffff:10.1.2.3");
        assert_eq!(mapped.bit_len(), 128);
        assert_eq!(mapped.unmap(), addr("10.1.2.3"));
        assert_eq!(mapped.unmap().bit_len(), 32);
        // IPv4-compatible is not IPv4-mapped: core leaves it alone.
        let compatible = addr("::1.2.3.4");
        assert_eq!(compatible.unmap(), compatible);
        // An IPv6 address that is not mapped stays IPv6.
        let plain = addr("2001:db8::1");
        assert_eq!(plain.unmap(), plain);
        assert_eq!(addr("1.2.3.4").unmap(), addr("1.2.3.4"));
    }

    #[test]
    fn a_zone_makes_addresses_unequal_and_ranges_empty() {
        assert_ne!(addr("fe80::1%eth0"), addr("fe80::1"));
        assert_eq!(addr("fe80::1%eth0"), addr("fe80::1%eth0"));
        let link = match Prefix::parse("fe80::/10") {
            Some(prefix) => prefix,
            None => panic!("fe80::/10 parses"),
        };
        assert!(!link.contains(&addr("fe80::1%eth0")));
        assert!(link.contains(&addr("fe80::1")));
    }

    #[test]
    fn a_mapped_range_contains_only_mapped_queries() {
        // Both sides are 128 bits wide, so a mapped range does contain a mapped
        // query -- and contains nothing else: the same address unmapped is 32
        // bits, and widths must agree. That is why a range written in the mapped
        // form is useless rather than harmful: by the time a pattern consults
        // it, the query has been unmapped.
        let range = match Prefix::parse("::ffff:10.0.0.0/104") {
            Some(prefix) => prefix,
            None => panic!("parses"),
        };
        assert!(range.contains(&addr("::ffff:10.1.2.3")));
        assert!(!range.contains(&addr("::ffff:10.1.2.3").unmap()));
        assert!(!range.contains(&addr("::ffff:11.1.2.3")));
        assert!(!range.contains(&addr("10.1.2.3")));
        assert!(!range.contains(&addr("2001:db8::1")));
    }

    #[test]
    fn prefixes_mask_their_host_bits() {
        let range = match Prefix::parse("10.1.2.3/8") {
            Some(prefix) => prefix,
            None => panic!("parses"),
        };
        assert_eq!(range.addr(), &addr("10.0.0.0"));
        assert_eq!(range.bits(), 8);
        assert!(range.contains(&addr("10.255.255.255")));
        assert!(!range.contains(&addr("11.0.0.1")));

        let v6 = match Prefix::parse("2001:db8::1/32") {
            Some(prefix) => prefix,
            None => panic!("parses"),
        };
        assert_eq!(v6.addr(), &addr("2001:db8::"));
        assert!(v6.contains(&addr("2001:db8:ffff::1")));
        assert!(!v6.contains(&addr("2001:db9::1")));

        // A bit count inside a byte, and the two degenerate widths.
        let odd = match Prefix::parse("192.168.1.0/25") {
            Some(prefix) => prefix,
            None => panic!("parses"),
        };
        assert!(odd.contains(&addr("192.168.1.127")));
        assert!(!odd.contains(&addr("192.168.1.128")));
        let all = match Prefix::parse("::/0") {
            Some(prefix) => prefix,
            None => panic!("parses"),
        };
        assert!(all.contains(&addr("2001:db8::1")));
        assert!(!all.contains(&addr("1.2.3.4")));
    }

    #[test]
    fn prefix_parse_refuses_what_core_refuses() {
        for text in [
            "10.0.0.0",        // no slash
            "10.0.0.0/33",     // wider than the address
            "10.0.0.0/-1",     // negative
            "10.0.0.0/",       // no bits
            "/8",              // no address
            "10.0.0.0/8/8",    // two slashes
            "10.0.0.0/8 ",     // trailing space
            "fe80::1%eth0/64", // a range is not scoped
        ] {
            assert_eq!(Prefix::parse(text), None, "{text:?}");
        }
        // A leading "+" is what `strconv.Atoi` accepts.
        assert!(Prefix::parse("10.0.0.0/+8").is_some());
    }
}
