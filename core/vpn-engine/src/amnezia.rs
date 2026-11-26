//! Amnezia `vpn://` share keys: `vpn://` + base64url(qCompress(JSON)), where qCompress is a
//! 4-byte big-endian length followed by a zlib stream. Only connection keys are imported; a
//! "full access" key that carries the server's SSH login is refused.
use crate::{vless::VlessKey, wireguard};
use anyhow::{bail, ensure, Context, Result};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use serde_json::Value;
use std::{io::Read, net::Ipv6Addr};

pub(crate) const MAX_KEY: usize = 64 * 1024;
const MAX_JSON: usize = 1024 * 1024;

pub enum AmneziaKey {
    /// WireGuard or AmneziaWG configuration text, validated by `wireguard::parse`.
    WireGuard {
        config: String,
        label: Option<String>,
    },
    Vless(VlessKey),
}

fn decode_document(value: &str) -> Result<Value> {
    let encoded = value
        .trim()
        .strip_prefix("vpn://")
        .context("Expected a vpn:// key")?;
    ensure!(encoded.len() <= MAX_KEY, "vpn:// key is too large");
    let bytes = URL_SAFE_NO_PAD
        .decode(encoded.trim_end_matches('='))
        .map_err(|_| anyhow::anyhow!("vpn:// key is not valid base64url"))?;
    // qCompress output: declared length, then a zlib stream (header byte 0x78).
    let json = if bytes.len() > 6 && bytes[4] == 0x78 {
        let declared = u32::from_be_bytes([bytes[0], bytes[1], bytes[2], bytes[3]]) as usize;
        ensure!(declared <= MAX_JSON, "vpn:// key expands beyond 1 MiB");
        let mut out = Vec::with_capacity(declared);
        flate2::read::ZlibDecoder::new(&bytes[4..])
            .take(MAX_JSON as u64 + 1)
            .read_to_end(&mut out)
            .map_err(|_| anyhow::anyhow!("vpn:// key is not a valid compressed document"))?;
        ensure!(out.len() <= MAX_JSON, "vpn:// key expands beyond 1 MiB");
        out
    } else {
        bytes
    };
    serde_json::from_slice(&json)
        .map_err(|_| anyhow::anyhow!("vpn:// key does not contain an Amnezia configuration"))
}

fn text(value: &Value) -> String {
    match value {
        Value::String(s) => s.clone(),
        Value::Number(n) => n.to_string(),
        _ => String::new(),
    }
}

/// Rebuilds a `.conf` from AmneziaWG `last_config` fields when the raw text is absent.
fn config_from_fields(c: &Value) -> Result<String> {
    let field = |name: &str| text(&c[name]);
    ensure!(
        !field("client_priv_key").is_empty(),
        "Amnezia key has no client private key"
    );
    let mut conf = format!(
        "[Interface]\nPrivateKey = {}\nAddress = {}\n",
        field("client_priv_key"),
        field("client_ip")
    );
    if !field("mtu").is_empty() {
        conf += &format!("MTU = {}\n", field("mtu"));
    }
    for name in [
        "Jc",
        "Jmin",
        "Jmax",
        "S1",
        "S2",
        "S3",
        "S4",
        "H1",
        "H2",
        "H3",
        "H4",
        "I1",
        "I2",
        "I3",
        "I4",
        "I5",
        "HeaderProtectionKey",
        "ContentPaddingAddition",
        "RekeyAfterTime",
        "RekeyTimeout",
        "RejectAfterTime",
        "KeepaliveTimeout",
        "MaxHandshakeAttempts",
        "RandomTrailers",
        "DisableCookies",
    ] {
        let value = field(name);
        if !value.is_empty() {
            conf += &format!("{name} = {value}\n");
        }
    }
    conf += &format!("[Peer]\nPublicKey = {}\n", field("server_pub_key"));
    if !field("psk_key").is_empty() {
        conf += &format!("PresharedKey = {}\n", field("psk_key"));
    }
    let host = field("hostName");
    let endpoint = if host.contains(':') {
        format!("[{host}]")
    } else {
        host
    };
    conf += &format!("Endpoint = {}:{}\n", endpoint, field("port"));
    let allowed: Vec<String> = c["allowed_ips"]
        .as_array()
        .map(|a| a.iter().map(text).filter(|s| !s.is_empty()).collect())
        .unwrap_or_else(|| vec!["0.0.0.0/0".into()]);
    conf += &format!("AllowedIPs = {}\n", allowed.join(", "));
    if !field("persistent_keep_alive").is_empty() {
        conf += &format!("PersistentKeepalive = {}\n", field("persistent_keep_alive"));
    }
    Ok(conf)
}

/// The endpoint with brackets around its IPv6 host, or None when it needs none or is not one:
/// `2001:db8::1:51820` is `[2001:db8::1]:51820`. Only a host that is an IPv6 address and a port that
/// is a number are taken for one; whatever else is wrong with the endpoint is left for
/// `wireguard::parse`, which refuses it.
fn bracketed(endpoint: &str) -> Option<String> {
    if endpoint.starts_with('[') {
        return None;
    }
    let (host, port) = endpoint.rsplit_once(':')?;
    (host.parse::<Ipv6Addr>().is_ok()
        && !port.is_empty()
        && port.bytes().all(|b| b.is_ascii_digit()))
    .then(|| format!("[{host}]:{port}"))
}

/// A configuration text in which the `Endpoint` of the `[Peer]` section has brackets around an
/// IPv6 host. Amnezia writes it without them (`Endpoint = 2001:db8::1:51820`), as
/// `config_from_fields` knows, and `wireguard::parse` refuses that in a configuration file, where
/// the port cannot be told from the last group of the address. Here the endpoint always has a port,
/// so the last group is it. Nothing else is changed, and nothing else is let through.
fn with_bracketed_endpoint(config: &str) -> String {
    let mut in_peer = false;
    let mut text = String::with_capacity(config.len() + 2);
    for line in config.lines() {
        let item = wireguard::significant(line);
        if item.starts_with('[') {
            in_peer = item.eq_ignore_ascii_case("[peer]");
        } else if let Some((name, value)) = item.split_once('=').filter(|_| in_peer) {
            if let Some(fixed) =
                bracketed(value.trim()).filter(|_| name.trim().eq_ignore_ascii_case("endpoint"))
            {
                text.push_str(&format!("{} = {fixed}\n", name.trim()));
                continue;
            }
        }
        text.push_str(line);
        text.push('\n');
    }
    text
}

/// Adds `DNS = ...` right after the line that opens the `[Interface]` section. That line is found
/// as `wireguard::parse` finds it, whatever its case, spacing, comment or line ending: an exact
/// `"[Interface]\n"` search found nothing in a file with Windows line endings, a lower-case
/// header or a space after it, and the key's resolvers were dropped without a word. A
/// configuration without the section is refused.
fn with_resolvers(config: &str, dns: &[String]) -> Result<String> {
    let mut text = String::with_capacity(config.len() + 64);
    let mut added = false;
    for line in config.lines() {
        text.push_str(line);
        text.push('\n');
        if !added && wireguard::opens_interface(line) {
            text.push_str(&format!("DNS = {}\n", dns.join(", ")));
            added = true;
        }
    }
    ensure!(added, "The configuration has no [Interface] section");
    Ok(text)
}

pub fn decode(value: &str) -> Result<AmneziaKey> {
    let document = decode_document(value)?;
    ensure!(
        document.is_object(),
        "vpn:// key does not contain an Amnezia configuration"
    );
    for credential in ["userName", "password"] {
        ensure!(
            text(&document[credential]).is_empty(),
            "This is a full-access Amnezia key that contains the server's SSH login; Ghostlink only imports connection keys. Ask for a connection key instead."
        );
    }
    ensure!(
        document.get("api_key").is_none()
            && document.get("auth_data").is_none()
            && document.get("api_config").is_none(),
        "Amnezia subscription (API) keys are not supported"
    );
    let label = crate::outline::key_label(&text(&document["description"]));
    let containers = document["containers"]
        .as_array()
        .context("vpn:// key lists no servers")?;
    let wanted = text(&document["defaultContainer"]);
    let supported = |c: &&Value| {
        matches!(
            c["container"].as_str(),
            Some("amnezia-awg2" | "amnezia-awg" | "amnezia-wireguard" | "amnezia-xray")
        )
    };
    let container = containers
        .iter()
        .filter(supported)
        .find(|c| c["container"] == wanted.as_str())
        .or_else(|| containers.iter().find(supported))
        .with_context(|| {
            let names: Vec<String> = containers.iter().map(|c| text(&c["container"])).collect();
            format!("This vpn:// key only contains unsupported protocols ({}); supported are AmneziaWG, WireGuard and XRay", names.join(", "))
        })?;
    let (kind, protocol) = match container["container"].as_str() {
        Some("amnezia-xray") => ("xray", "xray"),
        Some("amnezia-wireguard") => ("wireguard", "wireguard"),
        _ => ("awg", "awg"),
    };
    let last_config = text(&container[protocol]["last_config"]);
    ensure!(
        !last_config.is_empty(),
        "vpn:// key has no connection configuration"
    );
    let client: Value = serde_json::from_str(&last_config)
        .context("vpn:// connection configuration is malformed")?;
    if kind == "xray" {
        return Ok(AmneziaKey::Vless(VlessKey::from_xray(&client, label)?));
    }
    let mut config = match client["config"].as_str() {
        Some(raw) if !raw.trim().is_empty() => raw.to_string(),
        _ => config_from_fields(&client)?,
    };
    config = with_bracketed_endpoint(&config);
    // Amnezia templates may leave DNS out of the text; use the key's resolvers when present.
    if !config
        .lines()
        .any(|l| l.trim().to_ascii_lowercase().starts_with("dns"))
    {
        let dns: Vec<String> = [text(&document["dns1"]), text(&document["dns2"])]
            .into_iter()
            .filter(|d| d.parse::<std::net::IpAddr>().is_ok())
            .collect();
        if !dns.is_empty() {
            config = with_resolvers(&config, &dns)?;
        }
    }
    let parsed = wireguard::parse(&config)?;
    if kind == "awg" && !parsed.is_amnezia() {
        bail!("AmneziaWG key has no obfuscation parameters");
    }
    Ok(AmneziaKey::WireGuard { config, label })
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::io::Write;

    /// Encodes like Amnezia's exporter: qCompress (length + zlib) then base64url without padding.
    pub(crate) fn encode(document: &Value) -> String {
        let json = serde_json::to_vec(document).unwrap();
        let mut zlib = flate2::write::ZlibEncoder::new(Vec::new(), flate2::Compression::new(8));
        zlib.write_all(&json).unwrap();
        let mut bytes = (json.len() as u32).to_be_bytes().to_vec();
        bytes.extend(zlib.finish().unwrap());
        format!("vpn://{}", URL_SAFE_NO_PAD.encode(bytes))
    }

    const PRIVATE: &str = "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=";
    const PUBLIC: &str = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=";

    fn awg_document() -> Value {
        let last = serde_json::json!({
            "client_priv_key": PRIVATE, "client_ip": "10.8.1.2", "server_pub_key": PUBLIC,
            "hostName": "203.0.113.5", "port": 41820, "mtu": "1376", "persistent_keep_alive": "25",
            "allowed_ips": ["0.0.0.0/0", "::/0"],
            "Jc": "4", "Jmin": "10", "Jmax": "50", "S1": "24", "S2": "28", "S3": "16", "S4": "12",
            "H1": "100-199", "H2": "200-299", "H3": "300-399", "H4": "400-499", "I1": "<r 16>"
        });
        serde_json::json!({
            "containers": [{"container": "amnezia-awg2", "awg": {"last_config": last.to_string(), "port": "41820", "transport_proto": "udp"}}],
            "defaultContainer": "amnezia-awg2", "description": "Amnezia Frankfurt", "dns1": "1.1.1.1", "dns2": "1.0.0.1", "hostName": "203.0.113.5"
        })
    }

    #[test]
    fn amneziawg_keys_decode_and_validate() {
        match decode(&encode(&awg_document())).unwrap() {
            AmneziaKey::WireGuard { config, label } => {
                let parsed = wireguard::parse(&config).unwrap();
                assert_eq!(parsed.kind(), "amneziawg");
                assert_eq!(parsed.peer.port, 41820);
                assert_eq!(parsed.dns.len(), 2);
                assert_eq!(label.as_deref(), Some("Amnezia Frankfurt"));
            }
            AmneziaKey::Vless(_) => panic!("expected AmneziaWG"),
        }
    }

    #[test]
    fn full_access_and_unsupported_keys_are_refused() {
        let mut admin = awg_document();
        admin["userName"] = "root".into();
        admin["password"] = "server-ssh-secret".into();
        let error = decode(&encode(&admin)).err().unwrap().to_string();
        assert!(error.contains("full-access") && !error.contains("server-ssh-secret"));
        let openvpn = serde_json::json!({"containers":[{"container":"amnezia-openvpn","openvpn":{"last_config":"{}"}}],"defaultContainer":"amnezia-openvpn"});
        assert!(decode(&encode(&openvpn)).is_err());
        let mut hooked = awg_document();
        let last = serde_json::json!({"config": format!("[Interface]\nPrivateKey = {PRIVATE}\nAddress = 10.8.1.2/32\nPostUp = sh evil\n[Peer]\nPublicKey = {PUBLIC}\nEndpoint = 203.0.113.5:1\nAllowedIPs = 0.0.0.0/0\n")});
        hooked["containers"][0]["awg"]["last_config"] = last.to_string().into();
        assert!(decode(&encode(&hooked)).is_err());
    }

    #[test]
    fn decompression_is_bounded() {
        let huge = serde_json::json!({"description": "x".repeat(2 * 1024 * 1024)});
        assert!(decode(&encode(&huge)).is_err());
        let mut bytes = (u32::MAX).to_be_bytes().to_vec();
        bytes.extend([0x78, 0x9c, 0x03, 0x00]);
        assert!(decode(&format!("vpn://{}", URL_SAFE_NO_PAD.encode(bytes))).is_err());
    }

    /// An AmneziaWG configuration text as the app exports it, without a DNS line: `header` opens
    /// the `[Interface]` section and `newline` ends every line.
    fn raw_config(header: &str, newline: &str) -> String {
        [
            header,
            &format!("PrivateKey = {PRIVATE}"),
            "Address = 10.8.1.2/32",
            "Jc = 4",
            "Jmin = 10",
            "Jmax = 50",
            "S1 = 24",
            "S2 = 28",
            "H1 = 100-199",
            "H2 = 200-299",
            "H3 = 300-399",
            "H4 = 400-499",
            "[Peer]",
            &format!("PublicKey = {PUBLIC}"),
            "Endpoint = 203.0.113.5:41820",
            "AllowedIPs = 0.0.0.0/0",
            "",
        ]
        .join(newline)
    }

    /// The resolvers (`dns1`, `dns2` of the key: 1.1.1.1 and 1.0.0.1) found in what `decode`
    /// returns for a key that carries `config` as its configuration text.
    fn resolvers_of(config: &str) -> Vec<String> {
        let last = serde_json::json!({ "config": config });
        let document = serde_json::json!({
            "containers": [{"container": "amnezia-awg", "awg": {"last_config": last.to_string()}}],
            "defaultContainer": "amnezia-awg", "description": "Amnezia Test",
            "dns1": "1.1.1.1", "dns2": "1.0.0.1"
        });
        match decode(&encode(&document)).unwrap() {
            AmneziaKey::WireGuard { config, .. } => wireguard::parse(&config)
                .unwrap()
                .dns
                .iter()
                .map(ToString::to_string)
                .collect(),
            AmneziaKey::Vless(_) => panic!("expected AmneziaWG"),
        }
    }

    /// The key's resolvers were put in with `replacen("[Interface]\n", ..)`, which found nothing,
    /// and so dropped them without a word, when the file had Windows line endings, a header in
    /// lower case, or a space after it (code review 2026-11-25, M3).
    #[test]
    fn the_keys_resolvers_reach_the_configuration_whatever_its_header_looks_like() {
        let resolvers = vec!["1.1.1.1".to_string(), "1.0.0.1".to_string()];
        for (header, newline) in [
            ("[Interface]", "\n"),
            ("[Interface]", "\r\n"),
            ("[interface]", "\n"),
            ("[INTERFACE]", "\r\n"),
            ("[Interface]  ", "\n"),
            ("\t[Interface] \t", "\r\n"),
            ("[Interface] # tunnel", "\n"),
            ("[Interface] ; tunnel", "\r\n"),
        ] {
            assert_eq!(
                resolvers_of(&raw_config(header, newline)),
                resolvers,
                "header {header:?}, line endings {newline:?}"
            );
        }
    }

    #[test]
    fn a_configuration_that_names_its_resolver_keeps_it() {
        for dns in ["DNS = 9.9.9.9", "dns=9.9.9.9", "  Dns =  9.9.9.9"] {
            let config = raw_config("[Interface]", "\r\n").replace(
                "Address = 10.8.1.2/32",
                &format!("Address = 10.8.1.2/32\r\n{dns}"),
            );
            assert_eq!(resolvers_of(&config), vec!["9.9.9.9"], "{dns}");
        }
    }

    /// The result of `decode` for a key whose configuration text has this `Endpoint` value, as
    /// the host and port it connects to.
    fn endpoint_of(endpoint: &str, newline: &str) -> Result<(String, u16)> {
        let config = raw_config("[Interface]", newline).replace(
            "Endpoint = 203.0.113.5:41820",
            &format!("Endpoint = {endpoint}"),
        );
        let last = serde_json::json!({ "config": config });
        let document = serde_json::json!({
            "containers": [{"container": "amnezia-awg", "awg": {"last_config": last.to_string()}}],
            "defaultContainer": "amnezia-awg", "description": "Amnezia Test"
        });
        match decode(&encode(&document))? {
            AmneziaKey::WireGuard { config, .. } => {
                let parsed = wireguard::parse(&config)?;
                Ok((parsed.peer.host, parsed.peer.port))
            }
            AmneziaKey::Vless(_) => panic!("expected AmneziaWG"),
        }
    }

    /// Amnezia writes an IPv6 endpoint without brackets, and 0.9.1 refused such keys: the port
    /// cannot be told from the last group of an address in a configuration file, but here the
    /// endpoint always has a port, so the last group is it. They import again, with the address
    /// in brackets, and every other check stays: a configuration file with the same endpoint is
    /// still refused, and so is anything that is not an IPv6 address and a port.
    #[test]
    fn an_ipv6_endpoint_without_brackets_imports_and_nothing_else_is_let_through() {
        for (endpoint, expected) in [
            ("2001:db8::1:51820", ("2001:db8::1", 51820)),
            // A port that is also a valid group of the address.
            ("2001:db8::5:443", ("2001:db8::5", 443)),
            ("2001:db8:0:0:0:0:0:1:8080", ("2001:db8::1", 8080)),
            ("[2001:db8::1]:51820", ("2001:db8::1", 51820)),
            ("203.0.113.5:41820", ("203.0.113.5", 41820)),
            ("vpn.example.com:41820", ("vpn.example.com", 41820)),
        ] {
            for newline in ["\n", "\r\n"] {
                let (host, port) = endpoint_of(endpoint, newline)
                    .unwrap_or_else(|e| panic!("{endpoint:?} {newline:?}: {e}"));
                let canonical = host
                    .parse::<Ipv6Addr>()
                    .map_or(host.clone(), |ip| ip.to_string());
                assert_eq!(
                    (canonical.as_str(), port),
                    expected,
                    "{endpoint:?} {newline:?}"
                );
            }
        }
        // With a comment after it.
        assert_eq!(
            endpoint_of("2001:db8::1:51820 # home", "\n").unwrap(),
            ("2001:db8::1".to_string(), 51820)
        );
        // Still refused: no port, a port that is no port, no IPv6 address, more than one endpoint.
        for bad in [
            "2001:db8::1",
            "2001:db8::1:0",
            "2001:db8::1:65536",
            "2001:db8::1:",
            "2001:db8::zz:51820",
            "2001:db8:1:2:3:4:5:6:7:51820",
            "[2001:db8::1]",
            "[2001:db8::1]:0",
            "vpn.example.com",
            ":51820",
        ] {
            assert!(endpoint_of(bad, "\n").is_err(), "accepted: {bad}");
        }
        let twice = raw_config("[Interface]", "\n")
            .replace("AllowedIPs", "Endpoint = 2001:db8::2:1\nAllowedIPs")
            .replace("203.0.113.5:41820", "2001:db8::1:51820");
        assert!(wireguard::parse(&with_bracketed_endpoint(&twice)).is_err());
        // Only the [Peer]'s endpoint is looked at; the setting in another section is not touched.
        let elsewhere = raw_config("[Interface]", "\n").replace(
            "Address = 10.8.1.2/32",
            "Address = 10.8.1.2/32\nEndpoint = 2001:db8::1:51820",
        );
        let kept = with_bracketed_endpoint(&elsewhere);
        assert!(kept.contains("Endpoint = 2001:db8::1:51820") && !kept.contains("[2001"));
        // A configuration file (not a vpn:// key) is as strict as it was.
        let file =
            raw_config("[Interface]", "\n").replace("203.0.113.5:41820", "2001:db8::1:51820");
        assert!(wireguard::parse(&file).is_err());
    }

    /// Without an `[Interface]` section there is nowhere to put them; the key is refused, as the
    /// configuration is by `wireguard::parse`.
    #[test]
    fn a_configuration_without_an_interface_section_is_refused() {
        let config = raw_config("", "\n").replacen("\n", "", 1);
        let last = serde_json::json!({ "config": config });
        let document = serde_json::json!({
            "containers": [{"container": "amnezia-awg", "awg": {"last_config": last.to_string()}}],
            "defaultContainer": "amnezia-awg", "dns1": "1.1.1.1"
        });
        assert!(decode(&encode(&document)).is_err());
    }
}
