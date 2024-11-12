# Manifold

Manifold is a process manager. It's directly inspired by [Solo](https://github.com/aarondfrancis/solo), [foreman](https://github.com/ddollar/foreman?tab=readme-ov-file), and [Overmind](https://github.com/DarthSim/overmind). Here's my problem with all of them:

- Solo is for PHP/Laravel only
- Foreman doesn't allow me to interact with processes independent from one another
- Overmind depends on tmux and I'd rather avoid that dependency

## Installation

Via Homebrew:

```
brew install blvrd/tap/manifold
```

You can find also download the binary directly from the [releases](https://github.com/blvrd/manifold/releases).


## Usage

Run `manifold` in your project. By default, the program expects a `Procfile.dev` to be present. Otherwise, you can pass `-f {filename}` to specify a Procfile.
