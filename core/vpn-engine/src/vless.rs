//! VLESS over REALITY or TLS (`vless://` share links and Xray client JSON). Plaintext VLESS is
//! rejected: VLESS itself does not encrypt.
use anyhow::{bail, ensure, Context, Result};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use percent_encoding::percent_decode_str;
use serde_json::Value;
use url::Url;

pub enum Security {
    Reality {
        public_key: String,
        short_id: String,
    },
    Tls,
}

// Deliberately not Debug: the UUID is the credential.
pub struct VlessKey {
    pub uuid: String,
    pub server: String,
    pub port: u16,
    pub flow: String,
    pub server_name: String,
    pub fingerprint: String,
    pub security: Security,
    pub label: Option<String>,
}

fn uuid(value: &str) -> Result<String> {
    let value = value.to_ascii_lowercase();
    let groups: Vec<&str> = value.split('-').collect();
    ensure!(
        groups.len() == 5
            && [8, 4, 4, 4, 12]
                .iter()
                .zip(&groups)
                .all(|(n, g)| g.len() == *n && g.bytes().all(|b| b.is_ascii_hexdigit())),
        "Invalid VLESS user ID"
    );
    Ok(value)
}

/// Maps uTLS fingerprint names, including Xray/Amnezia forms such as `hellochrome_120`, to the
/// names sing-box accepts.
pub fn fingerprint(value: &str) -> String {
    let lower = value.to_ascii_lowercase();
    let name = lower.strip_prefix("hello").unwrap_or(&lower);
    let base = name.split('_').next().unwrap_or("");
    match base {
        "chrome" | "firefox" | "edge" | "safari" | "360" | "qq" | "ios" | "android" | "random"
        | "randomized" => base.into(),
        "randomizednoalpn" => "randomized".into(),
        _ => "chrome".into(),
    }
}

/// Whether the text of a switch such as `allowInsecure=` says "off": nothing, `0` or `false` in
/// any case. Every other text, whatever it is (`1`, `true`, `TRUE`, `yes`, `on`), asks for what is
/// never allowed, so only a clear "off" is let through.
fn flag_is_off(value: &str) -> bool {
    matches!(value.to_ascii_lowercase().as_str(), "" | "0" | "false")
}

/// The same for a switch in a JSON document: absent, `null`, `false`, `0`, or text that is off.
fn json_flag_is_off(value: &Value) -> bool {
    match value {
        Value::Null => true,
        Value::Bool(on) => !on,
        Value::String(text) => flag_is_off(text),
        Value::Number(number) => number.as_u64() == Some(0),
        _ => false,
    }
}

fn host(value: &str) -> Result<String> {
    let value = value.trim_matches(['[', ']']);
    ensure!(
        !value.is_empty()
            && value.len() <= 253
            && value
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'-' | b':')),
        "Invalid VLESS server"
    );
    Ok(value.to_string())
}

impl VlessKey {
    #[allow(clippy::too_many_arguments)]
    fn build(
        id: &str,
        server: &str,
        port: u16,
        network: &str,
        security: &str,
        flow: &str,
        sni: &str,
        fp: &str,
        public_key: &str,
        short_id: &str,
        label: Option<String>,
    ) -> Result<Self> {
        ensure!(port != 0, "VLESS port is required");
        ensure!(
            matches!(network, "" | "tcp" | "raw"),
            "VLESS transport {network} is not supported; use TCP (raw) with REALITY or TLS"
        );
        ensure!(
            matches!(flow, "" | "xtls-rprx-vision"),
            "Unsupported VLESS flow {flow}"
        );
        let server = host(server)?;
        let server_name = if sni.is_empty() {
            server.clone()
        } else {
            host(sni)?
        };
        let security = match security {
            "reality" => {
                let raw = URL_SAFE_NO_PAD
                    .decode(public_key.trim_end_matches('='))
                    .ok()
                    .filter(|r| r.len() == 32)
                    .context("REALITY public key must be a base64url 32-byte key")?;
                ensure!(
                    short_id.len() <= 16
                        && short_id.len().is_multiple_of(2)
                        && short_id.bytes().all(|b| b.is_ascii_hexdigit()),
                    "REALITY short ID must be up to 16 hex characters"
                );
                Security::Reality {
                    public_key: URL_SAFE_NO_PAD.encode(raw),
                    short_id: short_id.to_ascii_lowercase(),
                }
            }
            "tls" => Security::Tls,
            "" | "none" => bail!("Unencrypted VLESS is not supported; use REALITY or TLS"),
            other => bail!("Unsupported VLESS security {other}"),
        };
        Ok(Self {
            uuid: uuid(id)?,
            server,
            port,
            flow: flow.into(),
            server_name,
            fingerprint: fingerprint(if fp.is_empty() { "chrome" } else { fp }),
            security,
            label,
        })
    }

    pub fn parse(value: &str) -> Result<Self> {
        let value = value.trim();
        ensure!(
            value.len() <= 8192 && value.starts_with("vless://"),
            "Expected a vless:// link"
        );
        let url = Url::parse(value).map_err(|_| anyhow::anyhow!("Malformed vless:// link"))?;
        let label = url
            .fragment()
            .and_then(|f| crate::outline::key_label(&percent_decode_str(f).decode_utf8_lossy()));
        let mut query = std::collections::BTreeMap::new();
        for (name, item) in url.query_pairs() {
            // The last value of a repeated parameter would win here, while another client reads
            // the first: `security=none&security=reality`, `allowInsecure=1&allowInsecure=0`.
            // Real clients never repeat one.
            ensure!(
                query.insert(name.to_string(), item.to_string()).is_none(),
                "Repeated vless:// parameter {name}"
            );
        }
        for (name, item) in &query {
            match name.as_str() {
                "type" | "security" | "sni" | "fp" | "pbk" | "sid" | "spx" | "flow"
                | "encryption" | "alpn" => {}
                "headerType" => {
                    ensure!(item == "none", "VLESS header obfuscation is not supported")
                }
                "allowInsecure" | "insecure" => {
                    ensure!(flag_is_off(item), "Insecure TLS is never allowed")
                }
                other => bail!("Unsupported vless:// parameter {other}"),
            }
        }
        ensure!(
            query
                .get("encryption")
                .is_none_or(|e| e.is_empty() || e == "none"),
            "Unsupported VLESS encryption"
        );
        let get = |name: &str| query.get(name).map(String::as_str).unwrap_or("");
        Self::build(
            &percent_decode_str(url.username()).decode_utf8_lossy(),
            url.host_str().context("vless:// link has no server")?,
            url.port().context("vless:// link has no port")?,
            get("type"),
            get("security"),
            get("flow"),
            get("sni"),
            get("fp"),
            get("pbk"),
            get("sid"),
            label,
        )
    }

    /// Maps the VLESS outbound of an Xray client configuration (as exported by Amnezia).
    pub fn from_xray(config: &Value, label: Option<String>) -> Result<Self> {
        let outbound = config["outbounds"]
            .as_array()
            .and_then(|list| list.iter().find(|o| o["protocol"] == "vless"))
            .context("Xray configuration has no VLESS outbound")?;
        let vnext = &outbound["settings"]["vnext"][0];
        let user = &vnext["users"][0];
        let stream = &outbound["streamSettings"];
        let security = stream["security"].as_str().unwrap_or("");
        let settings = if security == "reality" {
            &stream["realitySettings"]
        } else {
            &stream["tlsSettings"]
        };
        // Xray reads the names of its settings without regard to case.
        ensure!(
            settings.as_object().is_none_or(|fields| fields
                .iter()
                .filter(|(name, _)| name.eq_ignore_ascii_case("allowInsecure"))
                .all(|(_, value)| json_flag_is_off(value))),
            "Insecure TLS is never allowed"
        );
        let text = |v: &Value| v.as_str().unwrap_or("").to_string();
        ensure!(
            user["encryption"].as_str().is_none_or(|e| e == "none"),
            "Unsupported VLESS encryption"
        );
        Self::build(
            &text(&user["id"]),
            &text(&vnext["address"]),
            vnext["port"]
                .as_u64()
                .and_then(|p| u16::try_from(p).ok())
                .unwrap_or(0),
            stream["network"].as_str().unwrap_or("tcp"),
            security,
            &text(&user["flow"]),
            &text(&settings["serverName"]),
            &text(&settings["fingerprint"]),
            &text(&settings["publicKey"]),
            &text(&settings["shortId"]),
            label,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    const ID: &str = "8f3d2c1b-1a2b-4c3d-9e8f-0123456789ab";
    const PBK: &str = "Z84J2IelR9ch3k8VtlVhhs5ycBUlXA7wHBWcBrjqnAw";

    #[test]
    fn reality_links_parse() {
        let key = VlessKey::parse(&format!("vless://{ID}@203.0.113.7:443?type=tcp&security=reality&sni=www.googletagmanager.com&fp=hellochrome_120&pbk={PBK}&sid=0123abcd&flow=xtls-rprx-vision&encryption=none#Tokyo%20VLESS")).unwrap();
        assert_eq!(key.port, 443);
        assert_eq!(key.fingerprint, "chrome");
        assert_eq!(key.label.as_deref(), Some("Tokyo VLESS"));
        assert!(matches!(key.security, Security::Reality { .. }));
    }

    #[test]
    fn unsafe_or_unmappable_links_are_rejected() {
        for bad in [
            format!("vless://{ID}@h.example:443?security=none"),
            format!("vless://{ID}@h.example:443?security=tls&allowInsecure=1"),
            format!("vless://{ID}@h.example:443?security=reality&type=xhttp&pbk={PBK}"),
            format!("vless://{ID}@h.example:443?security=reality&type=kcp&pbk={PBK}"),
            format!("vless://{ID}@h.example:443?security=reality&pbk=short"),
            format!("vless://not-a-uuid@h.example:443?security=reality&pbk={PBK}"),
            format!("vless://{ID}@h.example:443?security=reality&pbk={PBK}&exec=1"),
        ] {
            assert!(VlessKey::parse(&bad).is_err(), "accepted: {bad}");
        }
    }

    #[test]
    fn amnezia_xray_outbound_maps() {
        let config = serde_json::json!({"inbounds":[{"protocol":"socks"}],"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"198.51.100.9","port":443,"users":[{"id":ID,"encryption":"none","flow":"xtls-rprx-vision"}]}]},"streamSettings":{"network":"raw","security":"reality","realitySettings":{"fingerprint":"hellochrome_120","serverName":"www.googletagmanager.com","publicKey":PBK,"shortId":"a1b2","spiderX":""}}}]});
        let key = VlessKey::from_xray(&config, Some("Amnezia XRay".into())).unwrap();
        assert_eq!(key.server, "198.51.100.9");
        assert_eq!(key.server_name, "www.googletagmanager.com");
    }

    fn link(query: &str) -> String {
        format!("vless://{ID}@203.0.113.7:443?{query}")
    }

    /// The link is read with the last value of a repeated parameter, but another client reads the
    /// first. Real clients never repeat one, so a link that does is refused (code review
    /// 2026-11-25, M3).
    #[test]
    fn repeated_parameters_are_refused() {
        let reality = format!("security=reality&pbk={PBK}");
        assert!(VlessKey::parse(&link(&reality)).is_ok());
        for repeated in [
            format!("{reality}&sni=a.example&sni=b.example"),
            format!("security=none&{reality}"),
            format!("{reality}&allowInsecure=1&allowInsecure=0"),
            format!("{reality}&allowInsecure=0&allowInsecure=1"),
            format!("{reality}&insecure=1&insecure=0"),
            format!("{reality}&flow=xtls-rprx-vision&flow="),
            // The same name once spelled with an escape.
            format!("{reality}&fp=chrome&%66p=firefox"),
        ] {
            assert!(VlessKey::parse(&link(&repeated)).is_err(), "{repeated}");
        }
    }

    /// Only a clear "off" lets the flag through: the old check refused "1" and "true" and let
    /// every other spelling of yes pass unnoticed.
    #[test]
    fn insecure_tls_is_refused_in_every_spelling_of_yes() {
        let reality = format!("security=reality&pbk={PBK}");
        for flag in ["allowInsecure", "insecure"] {
            for on in ["1", "true", "TRUE", "True", "yes", "on", "2", "enabled"] {
                let text = link(&format!("{reality}&{flag}={on}"));
                assert!(VlessKey::parse(&text).is_err(), "{flag}={on}");
            }
            for off in ["0", "false", "FALSE", "False", ""] {
                let text = link(&format!("{reality}&{flag}={off}"));
                assert!(VlessKey::parse(&text).is_ok(), "{flag}={off}");
            }
        }
    }

    #[test]
    fn xray_configurations_refuse_insecure_tls_in_any_form() {
        let outbound = |settings: serde_json::Value| serde_json::json!({"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"198.51.100.9","port":443,"users":[{"id":ID,"encryption":"none"}]}]},"streamSettings":{"network":"raw","security":"tls","tlsSettings":settings}}]});
        let key = |settings: serde_json::Value| VlessKey::from_xray(&outbound(settings), None);
        assert!(key(serde_json::json!({"serverName":"a.example"})).is_ok());
        for off in [
            serde_json::json!(false),
            serde_json::json!(null),
            serde_json::json!("false"),
            serde_json::json!("0"),
            serde_json::json!(0),
        ] {
            let settings = serde_json::json!({"serverName":"a.example","allowInsecure":off});
            assert!(key(settings).is_ok(), "{off}");
        }
        for on in [
            serde_json::json!(true),
            serde_json::json!("true"),
            serde_json::json!("TRUE"),
            serde_json::json!("1"),
            serde_json::json!(1),
            serde_json::json!("yes"),
            serde_json::json!([true]),
            serde_json::json!({}),
        ] {
            let settings = serde_json::json!({"serverName":"a.example","allowInsecure":on});
            assert!(key(settings).is_err(), "{on}");
        }
    }
}
