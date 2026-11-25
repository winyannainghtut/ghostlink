use crate::Args;
use anyhow::{ensure, Result};
use ghostlink_contracts::Stream;

#[cfg(windows)]
type NewInstance =
    fn(&str, &str, Rights, bool) -> Result<tokio::net::windows::named_pipe::NamedPipeServer>;

#[cfg(windows)]
pub struct Listener {
    /// The instance that waits for the next client; None after a replacement could not be made.
    pipe: Option<tokio::net::windows::named_pipe::NamedPipeServer>,
    endpoint: String,
    owner: String,
    rights: Rights,
    /// Makes an instance of the pipe (`create`); a field so a test can make it fail.
    new_instance: NewInstance,
}

/// What the owner of the pipe may do with it.
#[cfg(windows)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Rights {
    /// `GRGW`: read and write, and also `FILE_CREATE_PIPE_INSTANCE`, the right to start another
    /// server on the pipe's name (it is the same bit, 0x4, as `FILE_APPEND_DATA`, which
    /// `GENERIC_WRITE` stands for in an ACE). It is for a service that runs as the owner itself:
    /// it makes a new instance for every client and needs that right, and with nobody else
    /// involved there is nothing to guard.
    Full,
    /// `FILE_GENERIC_READ | FILE_GENERIC_WRITE` (0x0012019F) without that right: 0x0012019B. It
    /// is for the installed service, which runs as SYSTEM for a user: without it that user, or
    /// whatever runs as them, could start a second server on the pipe's name that clients, which
    /// check only who made the instance they reach, would accept. A client's own `GENERIC_READ |
    /// GENERIC_WRITE` does not ask for the bit on a pipe: the system turns it into exactly these
    /// rights, so every client, ours and the default ones of Node.js, .NET and Python, still
    /// connects.
    Narrow,
}

#[cfg(windows)]
impl Rights {
    fn sddl(self) -> &'static str {
        match self {
            Rights::Full => "GRGW",
            Rights::Narrow => "0x12019B",
        }
    }
}

#[cfg(windows)]
fn create(
    endpoint: &str,
    owner: &str,
    rights: Rights,
    first: bool,
) -> Result<tokio::net::windows::named_pipe::NamedPipeServer> {
    use windows_sys::Win32::{
        Foundation::LocalFree,
        Security::{
            Authorization::ConvertStringSecurityDescriptorToSecurityDescriptorW,
            SECURITY_ATTRIBUTES,
        },
    };
    let sddl: Vec<u16> = format!("D:P(A;;GA;;;SY)(A;;{};;;{owner})", rights.sddl())
        .encode_utf16()
        .chain(Some(0))
        .collect();
    // SAFETY: `sddl` is a NUL-terminated UTF-16 string that outlives the call. On success
    // `descriptor` is a LocalAlloc'd security descriptor; `attributes` points at it and lives on
    // this stack frame while the pipe is created (the call copies the security settings into the
    // kernel object), and `descriptor` is freed once afterwards, whatever the result. The failed
    // conversion returns before allocating.
    unsafe {
        let mut descriptor = std::ptr::null_mut();
        ensure!(
            ConvertStringSecurityDescriptorToSecurityDescriptorW(
                sddl.as_ptr(),
                1,
                &mut descriptor,
                std::ptr::null_mut()
            ) != 0,
            "Invalid pipe ACL"
        );
        let mut attributes = SECURITY_ATTRIBUTES {
            nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
            lpSecurityDescriptor: descriptor,
            bInheritHandle: 0,
        };
        let result = tokio::net::windows::named_pipe::ServerOptions::new()
            .first_pipe_instance(first)
            .reject_remote_clients(true)
            .max_instances(64)
            .create_with_security_attributes_raw(
                endpoint,
                (&mut attributes as *mut SECURITY_ATTRIBUTES).cast(),
            );
        LocalFree(descriptor);
        Ok(result?)
    }
}

#[cfg(windows)]
impl Listener {
    pub async fn bind(args: &Args) -> Result<Self> {
        ensure!(
            args.endpoint.starts_with(r"\\.\pipe\") && !args.endpoint[9..].contains(['\\', '/']),
            "Only local pipe endpoints are permitted"
        );
        let current = ghostlink_engine::storage::current_sid()?;
        let owner = args.owner_sid.clone().unwrap_or_else(|| current.clone());
        ensure!(
            owner.starts_with("S-1-")
                && owner.len() < 190
                && owner
                    .bytes()
                    .all(|b| b.is_ascii_digit() || matches!(b, b'S' | b'-')),
            "Invalid owner SID"
        );
        ensure!(
            current == owner || current == "S-1-5-18",
            "Only SYSTEM may configure access for a different user"
        );
        // SYSTEM serving another user keeps that user from starting a second server on the pipe;
        // a service that runs as the owner makes the instances itself, and needs the right to.
        let rights = if owner == current {
            Rights::Full
        } else {
            Rights::Narrow
        };
        let pipe = create(&args.endpoint, &owner, rights, true)?;
        Ok(Self {
            pipe: Some(pipe),
            endpoint: args.endpoint.clone(),
            owner,
            rights,
            new_instance: create,
        })
    }
    /// Cancel-safe: the instance that waits stays in `self` while this waits for a client.
    pub async fn accept(&mut self) -> Result<Stream> {
        if self.pipe.is_none() {
            // A replacement could not be made after the last client: make it first.
            self.pipe = Some((self.new_instance)(
                &self.endpoint,
                &self.owner,
                self.rights,
                false,
            )?);
        }
        let connected = match self.pipe.as_mut() {
            Some(pipe) => pipe.connect().await,
            None => anyhow::bail!("No pipe instance"),
        };
        if let Err(error) = connected {
            // A failed connect can leave this instance unusable. Replace it; the new one exists
            // before the old one closes, so the pipe name is never free.
            if let Ok(fresh) = (self.new_instance)(&self.endpoint, &self.owner, self.rights, false)
            {
                self.pipe = Some(fresh);
            }
            return Err(error.into());
        }
        // The connected instance goes to the caller, and a fresh one waits for the next client, so
        // the pipe name cannot be captured between clients. If none can be made now, the client
        // is served all the same (the connected instance keeps the name) and the next call tries
        // again: the connected instance must never stay here as the one that waits, or the next
        // call would hand the same, already used, connection out a second time.
        let connected = match self.pipe.take() {
            Some(pipe) => pipe,
            None => anyhow::bail!("No pipe instance"),
        };
        self.pipe = (self.new_instance)(&self.endpoint, &self.owner, self.rights, false).ok();
        Ok(Box::new(connected))
    }
}

#[cfg(unix)]
pub struct Listener {
    listener: tokio::net::UnixListener,
    owner: u32,
    path: std::path::PathBuf,
}
#[cfg(unix)]
impl Listener {
    pub async fn bind(args: &Args) -> Result<Self> {
        use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
        let path = std::path::PathBuf::from(&args.endpoint);
        ensure!(path.is_absolute(), "Socket path must be absolute");
        let parent = path
            .parent()
            .ok_or_else(|| anyhow::anyhow!("Invalid socket directory"))?;
        std::fs::create_dir_all(parent)?;
        let meta = std::fs::symlink_metadata(parent)?;
        let uid = ghostlink_engine::storage::effective_uid();
        ensure!(
            !meta.file_type().is_symlink() && meta.uid() == uid && meta.mode() & 0o022 == 0,
            "Socket directory must be owned by the daemon and not writable by other users"
        );
        let owner = args.owner_uid.unwrap_or(uid);
        ensure!(
            uid == 0 || owner == uid,
            "Only root may authorize another UID"
        );
        if let Ok(meta) = std::fs::symlink_metadata(&path) {
            ensure!(
                meta.file_type().is_socket(),
                "Refusing to remove a non-socket endpoint"
            );
            ensure!(
                tokio::net::UnixStream::connect(&path).await.is_err(),
                "Another service owns the socket"
            );
            std::fs::remove_file(&path)?;
        }
        let listener = tokio::net::UnixListener::bind(&path)?;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))?;
        use std::os::unix::ffi::OsStrExt;
        let c_path = std::ffi::CString::new(path.as_os_str().as_bytes())?;
        ensure!(
            // SAFETY: `c_path` is a NUL-terminated path that outlives the call, which only reads
            // it; a group of u32::MAX (-1) leaves the group as it is.
            unsafe { libc::chown(c_path.as_ptr(), owner, u32::MAX) } == 0,
            "Cannot assign socket owner"
        );
        Ok(Self {
            listener,
            owner,
            path,
        })
    }
    pub async fn accept(&mut self) -> Result<Stream> {
        loop {
            let (stream, _) = self.listener.accept().await?;
            let uid = stream.peer_cred()?.uid();
            if uid == self.owner || uid == 0 {
                return Ok(Box::new(stream));
            }
        }
    }
}
#[cfg(unix)]
impl Drop for Listener {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.path);
    }
}

/// The real named pipe, the real client (`ghostlink_contracts::connect`) and the real kernel
/// access check: no stand-ins.
#[cfg(all(test, windows))]
mod tests {
    use super::*;
    use clap::Parser;
    use ghostlink_contracts::{read_frame, write_frame, Action, Request, Response, SCHEMA_VERSION};
    use serde_json::json;
    use std::{
        sync::atomic::{AtomicUsize, Ordering},
        time::Duration,
    };
    use tokio::{
        io::BufReader,
        net::windows::named_pipe::{ClientOptions, NamedPipeServer, ServerOptions},
    };

    /// ERROR_ACCESS_DENIED
    const DENIED: i32 = 5;

    fn endpoint(name: &str) -> String {
        format!(r"\\.\pipe\ghostlink-ipc-test-{}-{name}", std::process::id())
    }

    async fn listener(endpoint: &str) -> Listener {
        let args = Args::parse_from(["ghostlinkd", "--endpoint", endpoint]);
        Listener::bind(&args).await.unwrap()
    }

    fn me() -> String {
        ghostlink_engine::storage::current_sid().unwrap()
    }

    /// Reads one request from `stream` and answers it.
    async fn serve(stream: Stream, answer: &str) {
        let mut reader = BufReader::new(stream);
        read_frame(&mut reader).await.unwrap();
        let response = Response::ok(json!({ "answer": answer }));
        write_frame(reader.get_mut(), &response).await.unwrap();
    }

    async fn ask(endpoint: &str) -> Response {
        let request = Request {
            schema_version: SCHEMA_VERSION,
            agent_token: None,
            action: Action::Status {},
        };
        ghostlink_contracts::call(endpoint, request, Duration::from_secs(10)).await
    }

    fn denied(error: &anyhow::Error) -> bool {
        error
            .downcast_ref::<std::io::Error>()
            .and_then(std::io::Error::raw_os_error)
            == Some(DENIED)
    }

    /// A service that runs as the owner makes an instance for every client, also while the
    /// clients before them are still being served, as the service's accept loop does: three
    /// clients that arrive together are all served. (The narrow rights, which the owner of the
    /// pipe must not have when it is the service itself, left it unable to make the next
    /// instance, and the second client found no pipe.)
    #[tokio::test]
    async fn a_service_that_is_the_owner_serves_clients_that_arrive_together() {
        let endpoint = endpoint("serve");
        let mut listener = listener(&endpoint).await;
        let clients: Vec<_> = (0..3)
            .map(|_| {
                let endpoint = endpoint.clone();
                tokio::spawn(async move { ask(&endpoint).await })
            })
            .collect();
        let mut handlers = vec![];
        let until = std::time::Instant::now() + Duration::from_secs(10);
        while handlers.len() < clients.len() && std::time::Instant::now() < until {
            // Bounded: if a client never comes, this must fail, not wait for it for ever.
            let accepted = tokio::time::timeout(Duration::from_secs(5), listener.accept()).await;
            match accepted {
                // Each is served slowly, so the next client comes while this one is being served.
                Ok(Ok(stream)) => handlers.push(tokio::spawn(async move {
                    tokio::time::sleep(Duration::from_millis(300)).await;
                    serve(stream, "together").await
                })),
                Ok(Err(_)) => tokio::time::sleep(Duration::from_millis(50)).await,
                Err(_) => break,
            }
        }
        for client in clients {
            let response = tokio::time::timeout(Duration::from_secs(10), client)
                .await
                .expect("a client was never served")
                .unwrap();
            assert!(response.success, "{:?}", response.error.map(|e| e.message));
            assert_eq!(response.data.unwrap()["answer"], "together");
        }
        for handler in handlers {
            handler.await.unwrap();
        }
    }

    /// The DACL of a pipe instance as SDDL text, read from the instance itself.
    fn dacl_of(pipe: &NamedPipeServer) -> String {
        use std::os::windows::io::AsRawHandle;
        use windows_sys::Win32::{
            Foundation::LocalFree,
            Security::{
                Authorization::{
                    ConvertSecurityDescriptorToStringSecurityDescriptorW, GetSecurityInfo,
                    SE_KERNEL_OBJECT,
                },
                DACL_SECURITY_INFORMATION,
            },
        };
        // SAFETY: the pipe handle stays open for the whole block (`pipe` is borrowed). On success
        // GetSecurityInfo sets `descriptor` to a LocalAlloc'd security descriptor, which is
        // converted to a LocalAlloc'd NUL-terminated string (the loop stops at the NUL) that is
        // read only while allocated. Both are freed once, after their last use.
        unsafe {
            let mut descriptor = std::ptr::null_mut();
            let status = GetSecurityInfo(
                pipe.as_raw_handle() as _,
                SE_KERNEL_OBJECT,
                DACL_SECURITY_INFORMATION,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                &mut descriptor,
            );
            assert_eq!(status, 0, "cannot read the pipe's ACL");
            let mut text = std::ptr::null_mut();
            let converted = ConvertSecurityDescriptorToStringSecurityDescriptorW(
                descriptor,
                1,
                DACL_SECURITY_INFORMATION,
                &mut text,
                std::ptr::null_mut(),
            );
            assert_ne!(converted, 0);
            let mut len = 0;
            while *text.add(len) != 0 {
                len += 1;
            }
            let sddl = String::from_utf16_lossy(std::slice::from_raw_parts(text, len));
            LocalFree(text.cast());
            LocalFree(descriptor);
            sddl
        }
    }

    /// What the rights are, read back from the real pipe: SYSTEM keeps everything (it is the
    /// service, and makes the instances), the owner has exactly the rights of `Rights::Narrow`
    /// (read and write the data, read and write attributes and extended attributes, read the
    /// ACL, wait: no 0x4) or `Full` (the same and 0x4, as `FILE_GENERIC_WRITE` has it), and the
    /// ACL is protected and names nobody else.
    #[tokio::test]
    async fn the_acl_of_the_pipe_says_what_the_rights_say() {
        let me = me();
        let mask = |sddl: &str, who: &str| -> u32 {
            let ace = sddl
                .split('(')
                .find(|ace| ace.ends_with(&format!(";{who})")) || ace.contains(&format!(";{who})")))
                .unwrap_or_else(|| panic!("no entry for {who} in {sddl}"));
            let rights = ace.split(';').nth(2).unwrap();
            match rights {
                "GA" | "FA" => 0x1f01ff,
                hex => u32::from_str_radix(hex.trim_start_matches("0x"), 16)
                    .unwrap_or_else(|_| panic!("rights {hex} of {who} in {sddl}")),
            }
        };
        for (rights, expected) in [(Rights::Narrow, 0x12019b), (Rights::Full, 0x12019f)] {
            let pipe = create(&endpoint(&format!("acl-{rights:?}")), &me, rights, true).unwrap();
            let sddl = dacl_of(&pipe);
            assert!(sddl.starts_with("D:P"), "protected: {sddl}");
            assert_eq!(sddl.matches('(').count(), 2, "nobody else: {sddl}");
            assert_eq!(mask(&sddl, "SY"), 0x1f01ff, "SYSTEM is the service: {sddl}");
            assert_eq!(mask(&sddl, &me), expected, "{rights:?}: {sddl}");
            assert_eq!(
                mask(&sddl, &me) & 0x4 != 0,
                rights == Rights::Full,
                "FILE_CREATE_PIPE_INSTANCE: {sddl}"
            );
        }
    }

    /// With `GRGW` the owner could start a second server on the pipe's name (a client checks
    /// only who made the instance it reaches). With the narrow rights, which the installed
    /// service (SYSTEM, serving a user) uses, it cannot; `Full` still can, as a service that is
    /// the owner must.
    #[tokio::test]
    async fn the_owner_cannot_start_a_second_server_with_the_narrow_rights() {
        let second = |endpoint: &str| {
            ServerOptions::new()
                .first_pipe_instance(false)
                .reject_remote_clients(true)
                .max_instances(64)
                .create(endpoint)
        };
        let narrow = endpoint("narrow");
        let _service = create(&narrow, &me(), Rights::Narrow, true).unwrap();
        let Err(error) = second(&narrow) else {
            panic!("the owner started a second server");
        };
        assert_eq!(error.raw_os_error(), Some(DENIED), "{error}");
        let full = endpoint("full");
        let _service = create(&full, &me(), Rights::Full, true).unwrap();
        assert!(
            second(&full).is_ok(),
            "a service that is the owner makes instances"
        );
    }

    /// The narrow rights cost no client its access: ours (through `ghostlink_contracts`) and one
    /// that asks for plain `GENERIC_READ | GENERIC_WRITE` (what tokio's default, Node.js, .NET and
    /// Python ask for) open the pipe and are served.
    #[tokio::test]
    async fn the_owner_is_served_through_the_narrow_rights() {
        let endpoint = endpoint("clients");
        let service = create(&endpoint, &me(), Rights::Narrow, true).unwrap();
        // Our clients: the CLI, the MCP bridge and the desktop app all go through `call`.
        let ours = tokio::spawn({
            let endpoint = endpoint.clone();
            async move { ask(&endpoint).await }
        });
        service.connect().await.unwrap();
        serve(Box::new(service), "ours").await;
        let response = ours.await.unwrap();
        assert!(response.success, "{:?}", response.error.map(|e| e.message));
        assert_eq!(response.data.unwrap()["answer"], "ours");
        // A plain client on a fresh pipe of the same kind (the first one went with its client).
        let service = create(&endpoint, &me(), Rights::Narrow, true).unwrap();
        let plain = tokio::spawn({
            let endpoint = endpoint.clone();
            async move {
                let pipe = ClientOptions::new()
                    .open(&endpoint)
                    .expect("a plain client");
                let mut reader = BufReader::new(pipe);
                let request = Request {
                    schema_version: SCHEMA_VERSION,
                    agent_token: None,
                    action: Action::Status {},
                };
                write_frame(reader.get_mut(), &request).await.unwrap();
                read_frame(&mut reader).await.unwrap()
            }
        });
        service.connect().await.unwrap();
        serve(Box::new(service), "plain").await;
        let answer: Response = serde_json::from_slice(&plain.await.unwrap()).unwrap();
        assert_eq!(answer.data.unwrap()["answer"], "plain");
    }

    /// Whoever is not the owner (or SYSTEM) cannot open the pipe at all: the pipe here belongs to
    /// another account (a made-up SID), and this process, with its own, is a stranger to it.
    #[tokio::test]
    async fn only_the_owner_can_open_the_pipe() {
        for rights in [Rights::Full, Rights::Narrow] {
            let endpoint = endpoint(&format!("stranger-{rights:?}"));
            let _other = create(
                &endpoint,
                "S-1-5-21-1111111111-2222222222-3333333333-1001",
                rights,
                true,
            )
            .unwrap();
            let Err(error) = ghostlink_contracts::connect(&endpoint).await else {
                panic!("a stranger opened the pipe");
            };
            assert!(denied(&error), "{rights:?}: {error}");
        }
    }

    /// A service that is not the owner binds with the narrow rights, and one that is, with the
    /// full: `bind` chooses by comparing the two.
    #[tokio::test]
    async fn the_rights_follow_who_runs_the_service() {
        let own = listener(&endpoint("bind-own")).await;
        assert_eq!(own.rights, Rights::Full);
    }

    static CREATED: AtomicUsize = AtomicUsize::new(0);

    /// The first instance made after the pipe is bound fails, as when the system is out of
    /// resources.
    fn fails_the_first_replacement(
        endpoint: &str,
        owner: &str,
        rights: Rights,
        first: bool,
    ) -> Result<NamedPipeServer> {
        if CREATED.fetch_add(1, Ordering::SeqCst) == 0 {
            anyhow::bail!("simulated: no pipe instance can be made");
        }
        create(endpoint, owner, rights, first)
    }

    /// A client that connected is served even when the instance that should wait for the next
    /// one cannot be made, and the next call makes it. Before, the call failed with the client's
    /// connection left in `self.pipe` as the instance that waits.
    #[tokio::test]
    async fn a_client_is_served_when_the_next_instance_cannot_be_made() {
        let endpoint = endpoint("replacement");
        let mut listener = listener(&endpoint).await;
        listener.new_instance = fails_the_first_replacement;
        let first = tokio::spawn({
            let endpoint = endpoint.clone();
            async move { ask(&endpoint).await }
        });
        serve(listener.accept().await.unwrap(), "first").await;
        assert_eq!(first.await.unwrap().data.unwrap()["answer"], "first");
        // No instance waits now. The next call makes one, and the next client is served by it
        // (it asks a moment later, so that the instance exists).
        let (_, second) = tokio::join!(
            async { serve(listener.accept().await.unwrap(), "second").await },
            async {
                tokio::time::sleep(Duration::from_millis(200)).await;
                ask(&endpoint).await
            }
        );
        assert_eq!(second.data.unwrap()["answer"], "second");
    }
}
