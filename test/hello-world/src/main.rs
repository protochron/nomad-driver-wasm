use std::env;
fn main() {
    let args: Vec<String> = env::args().collect();

    let mut name = "world";

    if args.len() == 2 {
        name = &args[1]
    }

    println!("Hello, {}!", name);
}
