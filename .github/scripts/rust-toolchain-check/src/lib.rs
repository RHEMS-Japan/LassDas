//! The crate check-rust-toolchain.sh builds and tests inside the runtime
//! image: a library, its unit test and its doc test, with no dependency.

/// Adds two numbers.
///
/// ```
/// assert_eq!(rust_toolchain_check::add(2, 3), 5);
/// ```
pub fn add(a: u32, b: u32) -> u32 {
    a + b
}

#[cfg(test)]
mod tests {
    #[test]
    fn adds() {
        assert_eq!(super::add(1, 2), 3);
    }
}
