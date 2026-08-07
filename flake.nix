{
  description = "Nagi reproducible task-runner MVP";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in {
      packages = forAllSystems (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.buildGoModule {
            pname = "nagi";
            version = "0.1.0";
            src = ./.;
            vendorHash = "sha256-5WaCZ29wuU/aP05IBHTM0WhELYrYoerGlIS3QxoXL5o=";
            subPackages = [ "cmd/nagi" ];
            nativeBuildInputs = [ pkgs.git pkgs.makeWrapper ];
            doCheck = true;
            postInstall = ''
              wrapProgram $out/bin/nagi \
                --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.git pkgs.gh ]}
            '';
          };
        });

      checks = forAllSystems (system: {
        nagi = self.packages.${system}.default;
      });

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/nagi";
        };
      });

      devShells = forAllSystems (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.mkShell {
            packages = [ pkgs.go pkgs.git pkgs.gh pkgs.sqlite pkgs.jq ];
            shellHook = ''
              echo "Nagi development shell (Xcode is detected from the host)"
            '';
          };
        });
    };
}
