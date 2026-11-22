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
		ModeImplement: ProfilePuckDB, ModeDiscuss: ProfilePuckDBDiscuss, ModeReview: ProfilePuckDBReview,
	}
	mactestProfiles = map[Mode]string{
		ModeImplement: ProfileMactest, ModeDiscuss: ProfileMactestDiscuss,
	}
)

// project is a legacy project whose workspace and repository share its
// name. It gets its own copy of profiles, so changing one project's
// Profiles changes no other project or later Defaults().
func project(name, title string, id int64, profiles map[Mode]string, configs ...string) Project {
	return Project{Name: name, Title: title, VikunjaID: id, Workspace: name, Repository: "sperano/" + name,
		AgentConfigs: configs, Profiles: maps.Clone(profiles)}
}

// projects lists vikunjaSpawners.projects in their legacy order, which
// decides a shared repository's PR Workspace (the first one wins).
func projects() []Project {
	cocovm3d := project("cocovm-3d", "CoCoVM 3D", 141, plainProfiles, stackRust, "rust-emulator")
	cocovm3d.Repository, cocovm3d.BaseRef = "sperano/cocovm", "feature/3d"
	portal := project("portal", "Portal", 48, imageBuildProfiles, "homelab")
	portal.Workspace, portal.Repository = "hollingsworth", "sperano/hollingsworth"
	briq := project("briq", "BRIQ", 45, plainProfiles, stackSwift)
	briq.Repository = "sperano/BRIQ"
	return []Project{
		project("hollingsworth", "Hollingsworth", 3, imageBuildProfiles, "homelab"),
		project("cocovm", "CoCoVM", 2, plainProfiles, stackRust, "rust-emulator"),
		cocovm3d,
		project("puckdb", "PuckDB", 6, puckdbProfiles, stackGo, "puckdb"),
		portal,
		project("dotfiles", "Dot Files", 1, plainProfiles),
		project("nhl", "NHL (Rust)", 5, plainProfiles, stackRust),
		project("nhl-api", "NHL API (Rust)", 52, plainProfiles, stackRust),
		project("nhl-api-go", "NHL API (Go)", 53, plainProfiles, stackGo),
		project("maurice-web", "Maurice Web", 90, plainProfiles, stackWeb),
		project("vigie", "vigie", 103, plainProfiles, stackGo),
		project("sillage", "Sillage", 136, plainProfiles, stackGo, stackWeb),
		project("lucarne", "Lucarne", 139, plainProfiles, stackGo, stackWeb),
		project("qix-rs", "Qix (Rust)", 140, plainProfiles, stackRust),
		project("dummy", "Dummy", 142, plainProfiles, stackGo),
		project("solitaire", "Solitaire", 137, mactestProfiles, stackSwift),
		briq,
		project("qix-swift", "Qix (Swift)", 143, plainProfiles, stackSwift),
		project("maurice-swift", "Maurice (Swift)", 145, plainProfiles, stackSwift),
		project("gordon", "Gordon", 147, plainProfiles),
	}
}
