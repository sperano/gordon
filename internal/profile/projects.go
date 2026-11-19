package profile

import "maps"

// Mode-to-profile maps shared by the legacy projects.
var (
	plainProfiles = map[Mode]string{
		ModeImplement: ProfileDefault, ModeDiscuss: ProfileDiscussDefault,
	}
	imageBuildProfiles = map[Mode]string{
		ModeImplement: ProfileImageBuild, ModeDiscuss: ProfileDiscuss,
	}
	puckdbProfiles = map[Mode]string{
		ModeImplement: ProfilePuckDB, ModeDiscuss: ProfilePuckDBDiscuss,
	}
)

// project is a legacy project whose workspace and repository share its
// name. It gets its own copy of profiles, so changing one project's
// Profiles changes no other project or later Defaults().
func project(name string, id int64, profiles map[Mode]string, configs ...string) Project {
	return Project{Name: name, VikunjaID: id, Workspace: name, Repository: "sperano/" + name,
		AgentConfigs: configs, Profiles: maps.Clone(profiles)}
}

// projects lists vikunjaSpawners.projects in their legacy order, which
// decides a shared repository's PR Workspace (the first one wins).
func projects() []Project {
	cocovm3d := project("cocovm-3d", 141, plainProfiles, stackRust, "rust-emulator")
	cocovm3d.Repository = "sperano/cocovm"
	portal := project("portal", 48, imageBuildProfiles, "homelab")
	portal.Workspace, portal.Repository = "hollingsworth", "sperano/hollingsworth"
	briq := project("briq", 45, plainProfiles, stackSwift)
	briq.Repository = "sperano/BRIQ"
	return []Project{
		project("hollingsworth", 3, imageBuildProfiles, "homelab"),
		project("cocovm", 2, plainProfiles, stackRust, "rust-emulator"),
		cocovm3d,
		project("puckdb", 6, puckdbProfiles, stackGo, "puckdb"),
		portal,
		project("dotfiles", 1, plainProfiles),
		project("nhl", 5, plainProfiles, stackRust),
		project("nhl-api", 52, plainProfiles, stackRust),
		project("nhl-api-go", 53, plainProfiles, stackGo),
		project("maurice-web", 90, plainProfiles, stackWeb),
		project("vigie", 103, plainProfiles, stackGo),
		project("sillage", 136, plainProfiles, stackGo, stackWeb),
		project("lucarne", 139, plainProfiles, stackGo, stackWeb),
		project("qix-rs", 140, plainProfiles, stackRust),
		project("dummy", 142, plainProfiles, stackGo),
		project("solitaire", 137, plainProfiles, stackSwift),
		briq,
		project("qix-swift", 143, plainProfiles, stackSwift),
		project("maurice-swift", 145, plainProfiles, stackSwift),
		project("gordon", 147, plainProfiles),
	}
}
