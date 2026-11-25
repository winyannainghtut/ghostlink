use anyhow::{ensure, Context, Result};
use ghostlink_contracts::private;
use std::{fs, io::Write, path::Path};

/// Remove temporary plaintext configuration on startup errors, cancellation and normal exit.
pub struct PrivateConfig(std::path::PathBuf);
impl PrivateConfig {
    pub fn write(path: &Path, bytes: &[u8]) -> Result<Self> {
        atomic_write(path, bytes)?;
        Ok(Self(path.into()))
    }
    pub fn path(&self) -> &Path {
        &self.0
    }
}
impl Drop for PrivateConfig {
    fn drop(&mut self) {
        let _ = fs::remove_file(&self.0);
    }
}

/// This process's effective user ID.
#[cfg(unix)]
pub fn effective_uid() -> u32 {
    // SAFETY: geteuid has no preconditions, cannot fail and reads no memory of ours.
    unsafe { libc::geteuid() }
}

/// Service-private state; never call this on an arbitrary user-supplied file.
///
/// # Errors
/// Fails if the directory cannot be created, if it exists but belongs to another, untrusted
/// identity (another user on Unix; anyone but this user, SYSTEM or Administrators on Windows),
/// or if its permissions (mode 0700) or private ACL cannot be set.
pub fn private_dir(path: &Path) -> Result<()> {
    // `symlink_metadata`, not `exists`: `exists` follows a link, so a link whose target is missing
    // looked like nothing at all and skipped every check below.
    match fs::symlink_metadata(path) {
        Ok(_) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            if let Some(parent) = path.parent() {
                fs::create_dir_all(parent)?;
            }
            // Born private (mode 0700, or a protected ACL), not restricted a moment later. If
            // another process made the name first, whatever it made is checked below like any
            // directory that was already there.
            match private::create_dir(path) {
                Ok(()) => {}
                Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {}
                Err(error) => return Err(error.into()),
            }
        }
        Err(error) => return Err(error.into()),
    }
    let meta = fs::symlink_metadata(path)?;
    ensure!(
        !meta.file_type().is_symlink(),
        "State directory must not be a symlink"
    );
    ensure!(meta.is_dir(), "State directory is not a directory");
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        ensure!(
            meta.file_attributes() & 0x400 == 0,
            "State directory must not be a reparse point"
        );
        check_owner(path)?;
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::{MetadataExt, PermissionsExt};
        ensure!(
            meta.uid() == effective_uid(),
            "State directory has a different owner"
        );
        fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
    }
    #[cfg(windows)]
    restrict_acl(path)?;
    Ok(())
}

#[cfg(windows)]
fn check_owner(path: &Path) -> Result<()> {
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::{
        Foundation::LocalFree,
        Security::{Authorization::*, OWNER_SECURITY_INFORMATION},
    };
    let name: Vec<u16> = path.as_os_str().encode_wide().chain(Some(0)).collect();
    let current = current_sid()?;
    // SAFETY: `name` is a NUL-terminated UTF-16 path that outlives the call. On success
    // GetNamedSecurityInfoW sets `descriptor` to a LocalAlloc'd security descriptor and `owner`
    // to a SID inside it; both are read only before `descriptor` is freed, once, below. The
    // string ConvertSidToStringSidW returns is NUL-terminated (the loop stops at the NUL), read
    // only while allocated, and freed once. Every early return happens before an allocation.
    unsafe {
        let mut owner = std::ptr::null_mut();
        let mut descriptor = std::ptr::null_mut();
        ensure!(
            GetNamedSecurityInfoW(
                name.as_ptr(),
                SE_FILE_OBJECT,
                OWNER_SECURITY_INFORMATION,
                &mut owner,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                &mut descriptor
            ) == 0,
            "Cannot inspect state directory owner"
        );
        let mut sid = std::ptr::null_mut();
        let converted = ConvertSidToStringSidW(owner, &mut sid);
        let allowed = if converted != 0 {
            let mut len = 0;
            while *sid.add(len) != 0 {
                len += 1;
            }
            let value = String::from_utf16_lossy(std::slice::from_raw_parts(sid, len));
            LocalFree(sid.cast());
            value == current || matches!(value.as_str(), "S-1-5-18" | "S-1-5-32-544")
        } else {
            false
        };
        LocalFree(descriptor);
        ensure!(
            allowed,
            "State directory belongs to another untrusted identity; refusing privileged access"
        );
    }
    Ok(())
}

pub fn atomic_write(path: &Path, bytes: &[u8]) -> Result<()> {
    let temporary = path.with_extension("pending");
    // A stale temporary file never causes following a link or overwriting another file. The file
    // is private from the moment it exists (mode 0600, or a protected ACL on Windows), not made
    // private after the folder's inherited permissions applied to it.
    let mut file = private::create_file(&temporary).context(
        "Cannot create private transaction file; inspect stale .pending files after a crash",
    )?;
    let result = (|| {
        file.write_all(bytes)?;
        file.sync_all()?;
        drop(file);
        #[cfg(windows)]
        {
            use std::os::windows::ffi::OsStrExt;
            use windows_sys::Win32::Storage::FileSystem::{
                MoveFileExW, MOVEFILE_REPLACE_EXISTING, MOVEFILE_WRITE_THROUGH,
            };
            let from: Vec<u16> = temporary.as_os_str().encode_wide().chain(Some(0)).collect();
            let to: Vec<u16> = path.as_os_str().encode_wide().chain(Some(0)).collect();
            ensure!(
                // SAFETY: both paths are NUL-terminated UTF-16 buffers that outlive the call,
                // which only reads them.
                unsafe {
                    MoveFileExW(
                        from.as_ptr(),
                        to.as_ptr(),
                        MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH,
                    )
                } != 0,
                "Atomic state replacement failed"
            );
        }
        #[cfg(unix)]
        {
            fs::rename(&temporary, path)?;
            if let Some(parent) = path.parent() {
                fs::File::open(parent)?.sync_all()?;
            }
        }
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

#[cfg(windows)]
pub fn current_sid() -> Result<String> {
    use windows_sys::Win32::{
        Foundation::*,
        Security::{Authorization::ConvertSidToStringSidW, *},
        System::Threading::*,
    };
    // SAFETY: the token handle is closed once, right after its last use. The first
    // GetTokenInformation call only reports the size needed; the buffer for the second holds at
    // least that many bytes and is made of usize words, so it is aligned for TOKEN_USER (which
    // holds pointers; a Vec<u8> need not be). TOKEN_USER is read only after that call succeeded,
    // and its SID points into `data`, which lives until the end of the block. The string
    // ConvertSidToStringSidW returns is NUL-terminated, read only while allocated, and freed once.
    unsafe {
        let mut token = std::ptr::null_mut();
        ensure!(
            OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) != 0,
            "Cannot inspect user token"
        );
        let mut size = 0;
        GetTokenInformation(token, TokenUser, std::ptr::null_mut(), 0, &mut size);
        let mut data = vec![0usize; (size as usize).div_ceil(std::mem::size_of::<usize>())];
        let ok = GetTokenInformation(token, TokenUser, data.as_mut_ptr().cast(), size, &mut size);
        CloseHandle(token);
        ensure!(ok != 0, "Cannot inspect user SID");
        let mut sid = std::ptr::null_mut();
        ensure!(
            ConvertSidToStringSidW((*(data.as_ptr().cast::<TOKEN_USER>())).User.Sid, &mut sid) != 0,
            "Cannot format user SID"
        );
        let mut len = 0;
        while *sid.add(len) != 0 {
            len += 1;
        }
        let result = String::from_utf16_lossy(std::slice::from_raw_parts(sid, len));
        LocalFree(sid.cast());
        Ok(result)
    }
}

#[cfg(windows)]
pub fn restrict_acl(path: &Path) -> Result<()> {
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::{
        Foundation::LocalFree,
        Security::{Authorization::*, *},
    };
    let sddl = format!("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;{})", current_sid()?);
    let wide: Vec<u16> = sddl.encode_utf16().chain(Some(0)).collect();
    let name: Vec<u16> = path.as_os_str().encode_wide().chain(Some(0)).collect();
    // SAFETY: `wide` and `name` are NUL-terminated UTF-16 buffers that outlive the calls. On
    // success `sd` is a LocalAlloc'd security descriptor, passed to SetFileSecurityW while it
    // is allocated and freed once afterwards; the failed conversion returns before allocating.
    unsafe {
        let mut sd = std::ptr::null_mut();
        ensure!(
            ConvertStringSecurityDescriptorToSecurityDescriptorW(
                wide.as_ptr(),
                1,
                &mut sd,
                std::ptr::null_mut()
            ) != 0,
            "Cannot create private ACL"
        );
        let ok = SetFileSecurityW(
            name.as_ptr(),
            DACL_SECURITY_INFORMATION | PROTECTED_DACL_SECURITY_INFORMATION,
            sd,
        );
        LocalFree(sd);
        ensure!(ok != 0, "Cannot protect state ACL");
    }
    Ok(())
}

#[cfg(windows)]
pub fn seal(bytes: &[u8]) -> Result<Vec<u8>> {
    dpapi(bytes, true)
}
#[cfg(windows)]
pub fn unseal(bytes: &[u8]) -> Result<Vec<u8>> {
    dpapi(bytes, false)
}
#[cfg(unix)]
pub fn seal(bytes: &[u8]) -> Result<Vec<u8>> {
    Ok(bytes.to_vec())
}
#[cfg(unix)]
pub fn unseal(bytes: &[u8]) -> Result<Vec<u8>> {
    Ok(bytes.to_vec())
}

#[cfg(windows)]
fn dpapi(bytes: &[u8], encrypt: bool) -> Result<Vec<u8>> {
    use windows_sys::Win32::{Foundation::LocalFree, Security::Cryptography::*};
    let input = CRYPT_INTEGER_BLOB {
        cbData: bytes.len().try_into()?,
        pbData: bytes.as_ptr() as *mut u8,
    };
    let mut output = CRYPT_INTEGER_BLOB {
        cbData: 0,
        pbData: std::ptr::null_mut(),
    };
    // SAFETY: `input` points at `bytes`, which outlives the call; DPAPI only reads it (the
    // `*mut` is the C struct's type). On success DPAPI sets `output` to a LocalAlloc'd buffer of
    // `cbData` bytes, which is copied out while allocated (a null or empty buffer is not turned
    // into a slice at all) and then freed once; on failure it allocates nothing and the function
    // returns before reading it.
    unsafe {
        let ok = if encrypt {
            CryptProtectData(
                &input,
                std::ptr::null(),
                std::ptr::null(),
                std::ptr::null(),
                std::ptr::null(),
                CRYPTPROTECT_UI_FORBIDDEN,
                &mut output,
            )
        } else {
            CryptUnprotectData(
                &input,
                std::ptr::null_mut(),
                std::ptr::null(),
                std::ptr::null(),
                std::ptr::null(),
                CRYPTPROTECT_UI_FORBIDDEN,
                &mut output,
            )
        };
        ensure!(
            ok != 0,
            "Windows DPAPI failed; state must be read by the identity that created it"
        );
        // An empty result (unsealing empty content) may come with a null pointer, which a slice
        // must never be made from, not even an empty one.
        let result = if output.pbData.is_null() || output.cbData == 0 {
            Vec::new()
        } else {
            std::slice::from_raw_parts(output.pbData, output.cbData as usize).to_vec()
        };
        if !output.pbData.is_null() {
            LocalFree(output.pbData.cast());
        }
        Ok(result)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn private_storage_round_trip() {
        let dir = tempfile::tempdir().unwrap();
        private_dir(dir.path()).unwrap();
        let path = dir.path().join("state");
        let sealed = seal(b"private material").unwrap();
        atomic_write(&path, &sealed).unwrap();
        assert_eq!(
            unseal(&fs::read(&path).unwrap()).unwrap(),
            b"private material"
        );
        atomic_write(&path, b"replacement").unwrap();
        assert_eq!(fs::read(&path).unwrap(), b"replacement");
    }

    /// Unsealing empty content gives empty content back (DPAPI may answer it with a null buffer,
    /// which must not become a slice).
    #[test]
    fn empty_content_round_trips() {
        assert_eq!(unseal(&seal(b"").unwrap()).unwrap(), b"");
    }

    /// A transaction file left by a crash is never followed or overwritten, and is not removed.
    #[test]
    fn a_stale_transaction_file_is_left_alone() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("state");
        fs::write(path.with_extension("pending"), b"stale").unwrap();
        assert!(atomic_write(&path, b"new").is_err());
        assert_eq!(fs::read(path.with_extension("pending")).unwrap(), b"stale");
        assert!(!path.exists());
    }

    #[test]
    fn a_new_directory_is_made_with_its_parents_and_a_file_is_refused() {
        let dir = tempfile::tempdir().unwrap();
        let nested = dir.path().join("a").join("b").join("state");
        private_dir(&nested).unwrap();
        assert!(nested.is_dir());
        // Again, as at every start of the service.
        private_dir(&nested).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(
                fs::metadata(&nested).unwrap().permissions().mode() & 0o777,
                0o700
            );
        }
        let file = dir.path().join("file");
        fs::write(&file, b"x").unwrap();
        assert!(private_dir(&file).is_err());
    }

    /// A link whose target is missing does not "exist" to `Path::exists`, which used to skip every
    /// check: it is a link all the same, and the state directory is never one.
    #[cfg(unix)]
    #[test]
    fn a_dangling_symlink_is_not_a_state_directory() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("elsewhere");
        let link = dir.path().join("state");
        std::os::unix::fs::symlink(&target, &link).unwrap();
        let error = private_dir(&link).unwrap_err().to_string();
        assert!(error.contains("symlink"), "{error}");
        assert!(!target.exists(), "the link was followed");
    }

    /// The same on Windows with a junction, which `cmd` can make without extra rights.
    #[cfg(windows)]
    #[test]
    fn a_dangling_junction_is_not_a_state_directory() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("elsewhere");
        let link = dir.path().join("state");
        fs::create_dir(&target).unwrap();
        let made = std::process::Command::new("cmd")
            .args(["/C", "mklink", "/J"])
            .arg(&link)
            .arg(&target)
            .output()
            .unwrap();
        assert!(made.status.success(), "mklink /J failed");
        fs::remove_dir(&target).unwrap();
        assert!(!link.exists(), "the junction should dangle");
        // Rust calls a junction a symlink, or the reparse check refuses it: either is a refusal
        // of the link as such, not the "already exists" that making a directory there gives.
        let error = private_dir(&link).unwrap_err().to_string();
        assert!(
            error.contains("symlink") || error.contains("reparse point"),
            "{error}"
        );
        assert!(!target.exists(), "the junction was followed");
        fs::remove_dir(&link).unwrap();
    }
}
