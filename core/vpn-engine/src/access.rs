//! One entry point for every connection credential the managed worker can use: Outline
//! `ss://`/`ssconf://`, `vless://`, Amnezia `vpn://`, and WireGuard/AmneziaWG `.conf` text.
use crate::{
    amnezia::{self, AmneziaKey},
    outline::{self, AccessKey},
    profile::{ProfileSummary, Remote},
    vless::{Security, VlessKey},
    wireguard::{self, WgConfig},
};
use anyhow::{ensure, Result};

/// The most text a credential may be: a WireGuard/AmneziaWG configuration
/// ([`wireguard::MAX_CONFIG`]) or an Amnezia `vpn://` key is the longest kind; `ss://`,
/// `ssconf://` and `vless://` keys are far shorter. A client that reads a credential from a file
/// or a pipe (the CLI; the desktop app's file picker has the same limit) reads up to this much and
/// leaves the verdict to the parsers, so it never refuses what they would take.
pub const MAX_CREDENTIAL: usize = 64 * 1024;

pub enum Outbound {
    Shadowsocks(AccessKey),
    Vless(VlessKey),
    /// WireGuard, or AmneziaWG when the configuration has obfuscation parameters.
    WireGuard(WgConfig),
}

pub struct Resolved {
    pub outbound: Outbound,
    pub label: Option<String>,
}

impl Outbound {
    /// Stored profile kind; also selects the outbound at connect time.
    pub fn kind(&self) -> &'static str {
        match self {
            Self::Shadowsocks(_) => "outline",
            Self::Vless(_) => "vless",
            Self::WireGuard(config) => config.kind(),
        }
    }
    /// Protocol name reported in connection status.
    pub fn protocol(&self) -> &'static str {
        match self {
            Self::Shadowsocks(_) => "shadowsocks",
            Self::Vless(key) => match key.security {
                Security::Reality { .. } => "vless-reality",
                Security::Tls => "vless-tls",
            },
            Self::WireGuard(config) => config.kind(),
        }
    }
    pub fn server(&self) -> (&str, u16, &'static str) {
        match self {
            Self::Shadowsocks(key) => (&key.server, key.port, "shadowsocks"),
            Self::Vless(key) => (&key.server, key.port, "tcp"),
            Self::WireGuard(config) => (&config.peer.host, config.peer.port, "udp"),
        }
    }
    pub fn full_tunnel(&self) -> bool {
        match self {
            Self::WireGuard(config) => config.full_tunnel(),
            _ => true,
        }
    }
    /// `fingerprint` must be keyed (see the daemon): summaries are visible to read-only agents.
    pub fn summary(
        &self,
        name: &str,
        fingerprint: String,
        label: Option<String>,
    ) -> ProfileSummary {
        if let Self::Shadowsocks(key) = self {
            let mut summary = key.summary(name, fingerprint);
            if let Some(label) = label {
                summary.display_name = label;
            }
            return summary;
        }
        let (host, port, protocol) = self.server();
        ProfileSummary {
            name: name.into(),
            kind: self.kind().into(),
            display_name: label.unwrap_or_else(|| name.into()),
            remotes: vec![Remote {
                host: host.into(),
                port,
                protocol: protocol.into(),
            }],
            tunnel_mode: if self.full_tunnel() { "full" } else { "split" }.into(),
            certificate_expires_unix: None,
            sha256: fingerprint,
            is_default: false,
        }
    }
}

/// True when `source` is a credential this module handles (as opposed to an OpenVPN profile).
pub fn handles(source: &str) -> bool {
    let source = source.trim();
    ["ss://", "ssconf://", "vless://", "vpn://"]
        .iter()
        .any(|p| source.starts_with(p))
        || wireguard::looks_like_config(source)
}

/// Parses and validates a credential. Dynamic `ssconf://` keys are fetched over HTTPS each time.
///
/// # Errors
/// Fails if the text is no credential this module handles, if it does not pass validation
/// (unsupported cipher, transport or option; malformed key or address), or if a dynamic key
/// cannot be fetched safely (an address that is not public, a redirect, a bad answer).
pub async fn resolve(source: &str) -> Result<Resolved> {
    let source = source.trim();
    if source.starts_with("ss://") || source.starts_with("ssconf://") {
        let key = outline::resolve(source).await?;
        let label = key.label.clone();
        return Ok(Resolved {
            outbound: Outbound::Shadowsocks(key),
            label,
        });
    }
    if source.starts_with("vless://") {
        let key = VlessKey::parse(source)?;
        let label = key.label.clone();
        return Ok(Resolved {
            outbound: Outbound::Vless(key),
            label,
        });
    }
    if source.starts_with("vpn://") {
        return Ok(match amnezia::decode(source)? {
            AmneziaKey::WireGuard { config, label } => Resolved {
                outbound: Outbound::WireGuard(wireguard::parse(&config)?),
                label,
            },
            AmneziaKey::Vless(key) => {
                let label = key.label.clone();
                Resolved {
                    outbound: Outbound::Vless(key),
                    label,
                }
            }
        });
    }
    ensure!(
        wireguard::looks_like_config(source),
        "Use an ss://, ssconf://, vless:// or vpn:// key, or a WireGuard/AmneziaWG configuration"
    );
    Ok(Resolved {
        outbound: Outbound::WireGuard(wireguard::parse(source)?),
        label: None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A client that reads a credential from a file or a pipe may refuse nothing a parser would
    /// take: the CLI used to stop at 16 KiB, below what the WireGuard and Amnezia parsers (and
    /// the desktop app's file picker) accept.
    #[test]
    fn the_credential_limit_covers_every_parser() {
        const { assert!(MAX_CREDENTIAL >= wireguard::MAX_CONFIG) };
        const { assert!(MAX_CREDENTIAL >= crate::amnezia::MAX_KEY) };
    }
}
