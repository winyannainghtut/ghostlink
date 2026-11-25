//! Files and directories that only this user can open, protected from the moment they exist.
//!
//! Creating a file with the default permissions and restricting it afterwards leaves a gap in
//! which the folder's inherited permissions apply: another account that can read the folder could
//! open the file in that gap, and a handle opened then stays valid after the permissions change.
//! Here the protection is part of the creation itself: mode 0600 (files) or 0700 (directories) on
//! Unix, and on Windows a protected ACL that gives SYSTEM and the current user full control and
//! nobody else any access, passed to `CreateFileW` / `CreateDirectoryW`.
//!
//! Both functions only ever create something NEW: they fail instead of opening, following or
//! replacing what is there, a link included (with `AlreadyExists` for another file or directory
//! of the same kind).

use std::{fs::File, io, path::Path};

/// Creates a NEW file for writing that only the current user can open (SYSTEM as well, on
/// Windows). On Windows no other process can open it while the returned handle lives.
///
/// # Errors
/// Fails with `AlreadyExists` if a file or link is at `path` (and with another error if a
/// directory is), and with the OS error if the file cannot be created.
pub fn create_file(path: &Path) -> io::Result<File> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(path)
    }
    #[cfg(windows)]
    {
        windows::create_file(path)
    }
}

/// Creates one NEW directory (its parent must exist) that only the current user can open, with
/// the same protection as [`create_file`]. On Windows its children inherit the protection.
///
/// # Errors
/// Fails with `AlreadyExists` if anything is at `path`, and with the OS error otherwise.
pub fn create_dir(path: &Path) -> io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new().mode(0o700).create(path)
    }
    #[cfg(windows)]
    {
        windows::create_dir(path)
    }
}

#[cfg(windows)]
mod windows {
    use std::{
        fs::File,
        io,
        os::windows::{ffi::OsStrExt, io::FromRawHandle},
        path::Path,
    };
    use windows_sys::Win32::{
        Foundation::{LocalFree, GENERIC_WRITE, INVALID_HANDLE_VALUE},
        Security::{
            Authorization::{
                ConvertSidToStringSidW, ConvertStringSecurityDescriptorToSecurityDescriptorW,
            },
            GetTokenInformation, TokenUser, SECURITY_ATTRIBUTES, TOKEN_QUERY, TOKEN_USER,
        },
        Storage::FileSystem::{CreateDirectoryW, CreateFileW, CREATE_NEW, FILE_ATTRIBUTE_NORMAL},
        System::Threading::{GetCurrentProcess, OpenProcessToken},
    };

    /// The SID of the user this process runs as, as text (`S-1-5-...`).
    pub(super) fn current_sid() -> io::Result<String> {
        // SAFETY: the token handle is closed once, right after its last use. The first
        // GetTokenInformation call only reports the size needed; the buffer for the second holds
        // at least that many bytes and is made of usize words, so it is aligned for TOKEN_USER
        // (which holds pointers). TOKEN_USER is read only after that call succeeded, and its SID
        // points into `data`, which lives until the end of the block. The string
        // ConvertSidToStringSidW returns is NUL-terminated (the loop stops at the NUL), read only
        // while allocated, and freed once.
        unsafe {
            let mut token = std::ptr::null_mut();
            if OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) == 0 {
                return Err(io::Error::last_os_error());
            }
            let mut size = 0;
            GetTokenInformation(token, TokenUser, std::ptr::null_mut(), 0, &mut size);
            let mut data = vec![0usize; (size as usize).div_ceil(std::mem::size_of::<usize>())];
            let read =
                GetTokenInformation(token, TokenUser, data.as_mut_ptr().cast(), size, &mut size);
            let error = io::Error::last_os_error();
            windows_sys::Win32::Foundation::CloseHandle(token);
            if read == 0 {
                return Err(error);
            }
            let mut sid = std::ptr::null_mut();
            if ConvertSidToStringSidW((*(data.as_ptr().cast::<TOKEN_USER>())).User.Sid, &mut sid)
                == 0
            {
                return Err(io::Error::last_os_error());
            }
            let mut len = 0;
            while *sid.add(len) != 0 {
                len += 1;
            }
            let text = String::from_utf16_lossy(std::slice::from_raw_parts(sid, len));
            LocalFree(sid.cast());
            Ok(text)
        }
    }

    /// Runs `create` with attributes that give SYSTEM and the current user full control, give
    /// nobody else any access, and take nothing from the parent folder (the DACL is protected).
    fn with_private_attributes<T>(
        create: impl FnOnce(*const SECURITY_ATTRIBUTES) -> io::Result<T>,
    ) -> io::Result<T> {
        let sddl: Vec<u16> = format!("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;{})", current_sid()?)
            .encode_utf16()
            .chain(Some(0))
            .collect();
        let mut descriptor = std::ptr::null_mut();
        // SAFETY: `sddl` is a NUL-terminated UTF-16 string that outlives the call. On success
        // `descriptor` is a LocalAlloc'd security descriptor; `attributes` points at it and lives
        // on this stack frame while `create` runs (the file and directory calls copy the security
        // settings into the new object), and `descriptor` is freed once afterwards, whatever the
        // result. The failed conversion returns before allocating.
        unsafe {
            if ConvertStringSecurityDescriptorToSecurityDescriptorW(
                sddl.as_ptr(),
                1,
                &mut descriptor,
                std::ptr::null_mut(),
            ) == 0
            {
                return Err(io::Error::last_os_error());
            }
            let attributes = SECURITY_ATTRIBUTES {
                nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
                lpSecurityDescriptor: descriptor,
                bInheritHandle: 0,
            };
            let result = create(&attributes);
            LocalFree(descriptor);
            result
        }
    }

    /// The path as the system calls take it: UTF-16, NUL-terminated. They take at most 260
    /// characters, unless an absolute path is in the extended `\\?\` form, which `std` uses by
    /// itself for long paths (and these calls do not), so a long absolute path is put in it.
    /// A path with `.` or `..` in it is left as it is: the extended form takes it literally.
    pub(super) fn wide(path: &Path) -> io::Result<Vec<u16>> {
        use std::path::Component;
        let mut name: Vec<u16> = path.as_os_str().encode_wide().collect();
        if name.contains(&0) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "path contains a NUL",
            ));
        }
        let extended: Vec<u16> = r"\\?\".encode_utf16().collect();
        let plain = path
            .components()
            .all(|c| !matches!(c, Component::ParentDir | Component::CurDir));
        if name.len() >= 240 && path.is_absolute() && plain && !name.starts_with(&extended) {
            let backslashes: Vec<u16> = name
                .iter()
                .map(|&c| {
                    if c == u16::from(b'/') {
                        u16::from(b'\\')
                    } else {
                        c
                    }
                })
                .collect();
            name = match backslashes.strip_prefix(&[u16::from(b'\\'), u16::from(b'\\')][..]) {
                // \\server\share\... becomes \\?\UNC\server\share\...
                Some(unc) => [
                    r"\\?\UNC\".encode_utf16().collect::<Vec<u16>>(),
                    unc.to_vec(),
                ]
                .concat(),
                None => [extended, backslashes].concat(),
            };
        }
        name.push(0);
        Ok(name)
    }

    pub(super) fn create_file(path: &Path) -> io::Result<File> {
        let name = wide(path)?;
        with_private_attributes(|attributes| {
            // SAFETY: `name` is a NUL-terminated UTF-16 path and `attributes` points at valid
            // attributes; both outlive the call, which only reads them. Sharing is off (0), so no
            // other process can open the file while this handle lives.
            let handle = unsafe {
                CreateFileW(
                    name.as_ptr(),
                    GENERIC_WRITE,
                    0,
                    attributes,
                    CREATE_NEW,
                    FILE_ATTRIBUTE_NORMAL,
                    std::ptr::null_mut(),
                )
            };
            if handle == INVALID_HANDLE_VALUE {
                return Err(io::Error::last_os_error());
            }
            // SAFETY: `handle` is a valid file handle just returned by CreateFileW, and nothing
            // else owns it, so the File takes ownership and closes it.
            Ok(unsafe { File::from_raw_handle(handle) })
        })
    }

    pub(super) fn create_dir(path: &Path) -> io::Result<()> {
        let name = wide(path)?;
        with_private_attributes(|attributes| {
            // SAFETY: `name` is a NUL-terminated UTF-16 path and `attributes` points at valid
            // attributes; both outlive the call, which only reads them.
            if unsafe { CreateDirectoryW(name.as_ptr(), attributes) } == 0 {
                return Err(io::Error::last_os_error());
            }
            Ok(())
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> std::path::PathBuf {
        let dir =
            std::env::temp_dir().join(format!("ghostlink-private-{}-{name}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    /// Never opens, follows or replaces anything that is already there.
    #[test]
    fn only_new_things_are_created() {
        let dir = scratch("new");
        let file = dir.join("secret.txt");
        drop(create_file(&file).unwrap());
        assert_eq!(
            create_file(&file).unwrap_err().kind(),
            io::ErrorKind::AlreadyExists
        );
        let folder = dir.join("private");
        create_dir(&folder).unwrap();
        assert_eq!(
            create_dir(&folder).unwrap_err().kind(),
            io::ErrorKind::AlreadyExists
        );
        // A directory is not a file, and a file is not a directory: refused, whatever the OS
        // calls it (Windows answers "access denied" to a file over a directory).
        assert!(create_file(&folder).is_err());
        assert_eq!(
            create_dir(&file).unwrap_err().kind(),
            io::ErrorKind::AlreadyExists
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The DACL of `path` as SDDL text, read by name (which needs no data access, so it works
    /// while the file is open without sharing).
    #[cfg(windows)]
    fn dacl_text(path: &Path) -> String {
        use std::os::windows::ffi::OsStrExt;
        use windows_sys::Win32::{
            Foundation::LocalFree,
            Security::{
                Authorization::{
                    ConvertSecurityDescriptorToStringSecurityDescriptorW, GetNamedSecurityInfoW,
                    SE_FILE_OBJECT,
                },
                DACL_SECURITY_INFORMATION,
            },
        };
        let name: Vec<u16> = path.as_os_str().encode_wide().chain(Some(0)).collect();
        // SAFETY: `name` is a NUL-terminated UTF-16 path that outlives the calls. On success
        // GetNamedSecurityInfoW sets `descriptor` to a LocalAlloc'd security descriptor, which is
        // converted to a LocalAlloc'd NUL-terminated string (the loop stops at the NUL) that is
        // read only while allocated. Both are freed once, after their last use.
        unsafe {
            let mut descriptor = std::ptr::null_mut();
            let status = GetNamedSecurityInfoW(
                name.as_ptr(),
                SE_FILE_OBJECT,
                DACL_SECURITY_INFORMATION,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                &mut descriptor,
            );
            assert_eq!(status, 0, "cannot read the ACL of {path:?}");
            let mut text = std::ptr::null_mut();
            let converted = ConvertSecurityDescriptorToStringSecurityDescriptorW(
                descriptor,
                1,
                DACL_SECURITY_INFORMATION,
                &mut text,
                std::ptr::null_mut(),
            );
            assert_ne!(converted, 0, "cannot write the ACL of {path:?} as text");
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

    /// The protection is there the moment the call returns, while the file is still open: a
    /// protected ACL with SYSTEM and this user and nobody else. A file made the ordinary way takes
    /// the folder's inherited ACL instead, which is what the old create-then-restrict order left in
    /// place until the next step.
    #[cfg(windows)]
    #[test]
    fn windows_acls_are_private_at_creation() {
        let dir = scratch("acl");
        let me = windows::current_sid().unwrap();
        let only_us = |sddl: &str| {
            sddl.starts_with("D:P")
                && sddl.matches('(').count() == 2
                && sddl.contains(";;;SY)")
                && sddl.contains(&format!(";;;{me})"))
        };

        let file = dir.join("secret.txt");
        let open = create_file(&file).unwrap();
        let text = dacl_text(&file);
        assert!(only_us(&text), "a new private file has {text}");
        drop(open);

        let plain = dir.join("plain.txt");
        drop(
            std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .open(&plain)
                .unwrap(),
        );
        let text = dacl_text(&plain);
        assert!(!only_us(&text), "an ordinary file has {text}");

        let folder = dir.join("private");
        create_dir(&folder).unwrap();
        let text = dacl_text(&folder);
        assert!(only_us(&text), "a new private directory has {text}");
        // What is made inside it inherits exactly that, and nothing else.
        let inner = folder.join("inner.txt");
        drop(std::fs::File::create_new(&inner).unwrap());
        let text = dacl_text(&inner);
        assert_eq!(text.matches('(').count(), 2, "{text}");
        assert!(
            text.contains(";;;SY)") && text.contains(&format!(";;;{me})")),
            "{text}"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// A path of more than 260 characters works, as it did when `std` created the files: the
    /// system calls used here do not take one unless it is in the extended form.
    #[cfg(windows)]
    #[test]
    fn long_paths_are_created_like_std_does() {
        let root = scratch("long");
        let mut deep = root.clone();
        while deep.as_os_str().len() < 300 {
            deep.push("a-folder-with-a-rather-long-name-in-it");
        }
        std::fs::create_dir_all(&deep).unwrap();
        let folder = deep.join("private");
        create_dir(&folder).unwrap();
        let file = folder.join("secret.txt");
        drop(create_file(&file).unwrap());
        assert!(file.as_os_str().len() > 260);
        assert!(std::fs::File::open(&file).is_ok());
        // Forward slashes and `.`/`..` are left to the caller as before: no surprise path.
        assert!(windows::wide(Path::new(r"C:\a\..\b")).is_ok());
        let _ = std::fs::remove_dir_all(&root);
    }

    /// No other process can open the file while it is being written: its content is never
    /// readable through a handle taken before the protection could apply.
    #[cfg(windows)]
    #[test]
    fn a_new_private_file_cannot_be_opened_while_it_is_open() {
        let dir = scratch("share");
        let file = dir.join("secret.txt");
        let open = create_file(&file).unwrap();
        assert!(std::fs::File::open(&file).is_err());
        drop(open);
        assert!(std::fs::File::open(&file).is_ok());
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// The protection is there the moment the call returns: Unix mode bits, right after creation
    /// and whatever the umask (the old way created with the default mode and changed it next).
    #[cfg(unix)]
    #[test]
    fn unix_modes_are_private_at_creation() {
        use std::os::unix::fs::PermissionsExt;
        let dir = scratch("mode");
        let file = dir.join("secret.txt");
        drop(create_file(&file).unwrap());
        let folder = dir.join("private");
        create_dir(&folder).unwrap();
        let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode(&file), 0o600);
        assert_eq!(mode(&folder), 0o700);
        let _ = std::fs::remove_dir_all(&dir);
    }
}
